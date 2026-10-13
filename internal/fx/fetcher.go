// Rate loading from external FX APIs (§8.7 fetch_fx_rates): HTTP fetch plus
// the INSERT ... ON CONFLICT upsert into fx.rates. The DB trigger on fx.rates
// publishes the fx_rate event by itself, so Go only inserts rows.
package fx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Scarvis/pet-backend/internal/common"
	"github.com/Scarvis/pet-backend/internal/db"
)

// Rate is one fx.rates row: the price of one unit of Base in Quote on AsOf.
type Rate struct {
	Base   string
	Quote  string
	AsOf   time.Time // date granularity; truncated to UTC midnight on write
	Rate   float64
	Source string
}

// validate rejects empty codes, non-positive rates and empty sources before
// the row reaches the database (the FK would otherwise surface an opaque error).
func (r Rate) validate() error {
	if strings.TrimSpace(r.Base) == "" || strings.TrimSpace(r.Quote) == "" {
		return common.BadRequest("base_code and quote_code must not be empty")
	}
	if r.Rate <= 0 {
		return common.BadRequest("rate must be positive")
	}
	if strings.TrimSpace(r.Source) == "" {
		return common.BadRequest("source must not be empty")
	}
	return nil
}

// UpsertRate writes one rate row, updating rate/source on conflict (§8.7 step 2).
// day is truncated to a UTC date so repeated loads for the same day collide on
// UNIQUE(base_code, quote_code, as_of) instead of inserting duplicates.
func UpsertRate(ctx context.Context, pool *pgxpool.Pool, r Rate) error {
	if err := r.validate(); err != nil {
		return common.Wrap(err)
	}
	day := r.AsOf.UTC().Truncate(24 * time.Hour)
	return db.WithSerializable(ctx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO fx.rates (base_code, quote_code, rate, as_of, source) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (base_code, quote_code, as_of) DO UPDATE SET rate = EXCLUDED.rate, source = EXCLUDED.source`,
			strings.ToUpper(strings.TrimSpace(r.Base)),
			strings.ToUpper(strings.TrimSpace(r.Quote)),
			r.Rate, day, r.Source)
		return common.Wrap(err)
	})
}

// UpsertRatesTx is the transaction-joined variant used by the cron handler so a
// whole daily load commits atomically. tx is the caller's transaction.
func UpsertRatesTx(ctx context.Context, tx pgx.Tx, rates []Rate) error {
	for _, r := range rates {
		if err := r.validate(); err != nil {
			return common.Wrap(err)
		}
		day := r.AsOf.UTC().Truncate(24 * time.Hour)
		_, err := tx.Exec(ctx, `INSERT INTO fx.rates (base_code, quote_code, rate, as_of, source) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (base_code, quote_code, as_of) DO UPDATE SET rate = EXCLUDED.rate, source = EXCLUDED.source`,
			strings.ToUpper(strings.TrimSpace(r.Base)),
			strings.ToUpper(strings.TrimSpace(r.Quote)),
			r.Rate, day, r.Source)
		if err != nil {
			return common.Wrap(err)
		}
	}
	return nil
}

// BuildRequestURL renders the GET URL for one base currency against an
// exchangerate.host-style API: {apiURL}/latest?base=USD&access_key=....
func BuildRequestURL(apiURL, apiKey, base string) (string, error) {
	if strings.TrimSpace(apiURL) == "" {
		return "", common.BadRequest("FX API URL is not configured")
	}
	if strings.TrimSpace(base) == "" {
		return "", common.BadRequest("base currency must not be empty")
	}
	u, err := url.Parse(strings.TrimRight(apiURL, "/") + "/latest")
	if err != nil {
		return "", common.Wrap(err)
	}
	q := u.Query()
	q.Set("base", strings.ToUpper(strings.TrimSpace(base)))
	if apiKey != "" {
		q.Set("access_key", apiKey)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ratesResponse mirrors the exchangerate.host /latest payload.
type ratesResponse struct {
	Base  string             `json:"base"`
	Date  string             `json:"date"`
	Rates map[string]float64 `json:"rates"`
	Error *struct {
		Code int    `json:"code"`
		Info string `json:"info"`
	} `json:"error"`
}

// ParseRatesResponse turns a /latest body for base into []Rate stamped with
// asOf and source. Non-positive quotes are skipped (never stored); an API-level
// error object is a hard error. Pure function — unit-tested without a network.
func ParseRatesResponse(body []byte, base string, asOf time.Time, source string) ([]Rate, error) {
	var resp ratesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, common.Wrap(fmt.Errorf("decode fx response: %w", err))
	}
	if resp.Error != nil {
		return nil, common.Wrap(fmt.Errorf("fx api error %d: %s", resp.Error.Code, resp.Error.Info))
	}
	if len(resp.Rates) == 0 {
		return nil, common.Wrap(common.BadRequest("fx response carries no rates"))
	}
	var out []Rate
	for quote, rate := range resp.Rates {
		if rate <= 0 {
			continue
		}
		out = append(out, Rate{
			Base:   strings.ToUpper(strings.TrimSpace(base)),
			Quote:  strings.ToUpper(strings.TrimSpace(quote)),
			AsOf:   asOf,
			Rate:   rate,
			Source: source,
		})
	}
	return out, nil
}

// FetchBaseRates GETs the /latest document for base with a stdlib HTTP client
// and parses it. HTTP happens outside any DB transaction (§8.0: no HTTP inside
// the SERIALIZABLE closure); the caller upserts the result afterwards.
func FetchBaseRates(ctx context.Context, client *http.Client, apiURL, apiKey, base string, asOf time.Time) ([]Rate, error) {
	u, err := BuildRequestURL(apiURL, apiKey, base)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, common.Wrap(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, common.Wrap(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, common.Wrap(err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, common.Wrap(fmt.Errorf("fx api status %d for base %s", resp.StatusCode, base))
	}
	return ParseRatesResponse(body, base, asOf, apiURL)
}
