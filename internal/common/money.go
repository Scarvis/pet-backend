package common

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
)

// Bigint is a 64-bit signed integer carried as a decimal string so it survives
// the GraphQL wire: GraphQL Int is 32-bit and JS numbers are exact only up to
// 2^53 (ARCHITECTURE section 1, 6.7). Over the API every *_minor value is a
// Bigint; Hasura returns bigint/numeric columns as strings
// (HASURA_GRAPHQL_STRINGIFY_NUMERIC_TYPES), and Go parses them with Int64().
type Bigint string

// Int64 parses the decimal string into an int64, rejecting out-of-range values.
func (b Bigint) Int64() (int64, error) { return strconv.ParseInt(string(b), 10, 64) }

// MarshalJSON renders the value as a quoted decimal string.
func (b Bigint) MarshalJSON() ([]byte, error) { return json.Marshal(string(b)) }

// UnmarshalJSON parses a quoted decimal string into b; non-string JSON is an
// error because *_minor must always travel as a string on the wire.
func (b *Bigint) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("invalid bigint %s: %w", data, err)
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid bigint %q: %w", s, err)
	}
	*b = Bigint(strconv.FormatInt(i, 10))
	return nil
}

// MinorUnits maps well-known currency codes to their minor_unit (decimal places).
// JPY has 0; BTC/ETH have many. Production code should prefer reading the value
// from fx.currencies.minor_unit at runtime; this map is a convenience for values
// not yet loaded or for quick conversions.
var MinorUnits = map[string]int{
	"JPY": 0, "BTC": 8, "ETH": 18,
}

func minorUnit(code string) int {
	if u, ok := MinorUnits[code]; ok {
		return u
	}
	return 2 // ISO 4217 default for most fiat currencies (e.g. USD, EUR)
}

// pow10 returns 10^n as a *big.Int for n >= 0.
func pow10(n int) *big.Int {
	v := big.NewInt(1)
	for i := 0; i < n; i++ {
		v.Mul(v, bigTen)
	}
	return v
}

var bigTen = big.NewInt(10)

// scaleOf returns 10^n as a *big.Float at high precision.
func scaleOf(n int) *big.Float { return new(big.Float).SetPrec(256).SetInt(pow10(n)) }

// ToMinor converts a human-readable amount into integer minor units for a
// currency with the given minor_unit. It uses big.Float to preserve precision
// for high-decimal currencies such as ETH (minor_unit 18), where naive float
// multiplication would lose significant digits.
func ToMinor(amount float64, minorUnit int) (int64, error) {
	if amount < 0 {
		return 0, fmt.Errorf("amount must be non-negative, got %v", amount)
	}
	f := new(big.Float).SetPrec(256).SetFloat64(amount)
	f.Mul(f, scaleOf(minorUnit))
	i, _ := f.Int(nil) // round to nearest integer
	return i.Int64(), nil
}

// FromMinor converts integer minor units back into a human-readable amount.
// Signed values are allowed because stored balances can be negative (debt).
func FromMinor(minor int64, minorUnit int) (float64, error) {
	f := new(big.Float).SetPrec(256).SetInt64(minor)
	f.Quo(f, scaleOf(minorUnit)) // divide by 10^minorUnit -> human units
	d, _ := f.Float64()
	return d, nil
}

// ConvertToMinor converts an amount expressed in the minor units of `from` to the
// integer minor units of `to`, applying rate where base=from and quote=to. It
// accounts for the differing minor_unit of both currencies: divide by 10^minorUnitFrom,
// multiply by rate, then scale up by 10^minorUnitTo.
func ConvertToMinor(minorFrom int64, rate float64, minorUnitFrom, minorUnitTo int) (int64, error) {
	if rate < 0 {
		return 0, fmt.Errorf("rate must be non-negative, got %v", rate)
	}
	f := new(big.Float).SetPrec(256).SetInt64(minorFrom)
	f.Quo(f, scaleOf(minorUnitFrom)) // human units of `from`
	f.Mul(f, big.NewFloat(rate))     // human units of `to`
	f.Mul(f, scaleOf(minorUnitTo))   // minor units of `to`
	i, _ := f.Int(nil)
	return i.Int64(), nil
}
