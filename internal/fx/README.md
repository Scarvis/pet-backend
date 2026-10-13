# fx — Currencies & Exchange Rates

Daily rate loading for multi-currency conversion. See `ARCHITECTURE.md` §8.7.

## Entry points

`Handler{pool, log, apiURL, apiKey, client}` + `NewHandler(pool, cfg, log)` (creds from `FX_API_URL` / `FX_API_KEY`, 30s HTTP client).

| Function | File | Notes |
|---|---|---|
| `FetchDailyRates(ctx, bases, asOf)` | `handler.go` | Cron + manual entry; fetches all bases first, then one `SERIALIZABLE` upsert |
| `UpsertRate(ctx, pool, Rate)` | `fetcher.go` | Single-row upsert |
| `UpsertRatesTx(ctx, tx, []Rate)` | `fetcher.go` | Tx-joined variant for atomic daily load |
| `FetchBaseRates(ctx, client, url, key, base, asOf)` | `fetcher.go` | HTTP GET, outside any DB tx |
| `ParseRatesResponse(body, base, asOf, source)` | `fetcher.go` | Pure JSON decode |
| `BuildRequestURL(url, key, base)` | `fetcher.go` | `{url}/latest?base=XXX&access_key=…` |

Routes: `POST /actions/fx/fetch` (manual), `POST /cron/fetch_fx_rates` (schedule `0 6 * * *`).

## Tables

`fx.rates` only (`UNIQUE(base_code, quote_code, as_of)`; upsert `DO UPDATE SET rate, source`). Codes upper-cased/trimmed; `asOf` truncated to UTC midnight so reloads are idempotent.

## Rules

- HTTP never runs inside the serializable closure; partial fetch failure aborts the whole load.
- `Rate{Base, Quote, AsOf, Rate, Source}` validated before DB (`Rate > 0`, non-empty codes/source) to avoid opaque FK errors.
- `ParseRatesResponse` hard-fails on `error{}`/empty rates/invalid JSON, skips `rate <= 0` quotes.
- The DB trigger on `fx.rates` publishes the `fx_rate` event itself — Go only inserts.

## Errors

`Wrap` for HTTP/status/parse/DB failures; validation errors for empty bases or bad rate rows.

## Tests

`fetcher_test.go` — URL building, response parsing, validation. Pure functions only.
