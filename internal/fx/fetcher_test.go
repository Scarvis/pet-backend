package fx

import (
	"testing"
	"time"
)

func TestBuildRequestURL(t *testing.T) {
	u, err := BuildRequestURL("https://api.exchangerate.host", "k", "usd")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://api.exchangerate.host/latest?access_key=k&base=USD"; u != want {
		t.Fatalf("got %q want %q", u, want)
	}
	if _, err := BuildRequestURL("", "k", "USD"); err == nil {
		t.Fatal("empty api url must fail")
	}
	if _, err := BuildRequestURL("https://x", "", ""); err == nil {
		t.Fatal("empty base must fail")
	}
}

func TestParseRatesResponse(t *testing.T) {
	body := []byte(`{"base":"USD","date":"2026-10-13","rates":{"EUR":0.9,"JPY":150.5,"ZERO":0}}`)
	asOf := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	rates, err := ParseRatesResponse(body, "usd", asOf, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(rates) != 2 {
		t.Fatalf("non-positive quotes must be skipped, got %d rates", len(rates))
	}
	for _, r := range rates {
		if r.Base != "USD" || r.Source != "test" || !r.AsOf.Equal(asOf) {
			t.Fatalf("bad rate stamp: %+v", r)
		}
	}
	if _, err := ParseRatesResponse([]byte(`{"error":{"code":101,"info":"bad key"}}`), "USD", asOf, "t"); err == nil {
		t.Fatal("api error object must fail")
	}
	if _, err := ParseRatesResponse([]byte(`{"base":"USD","rates":{}}`), "USD", asOf, "t"); err == nil {
		t.Fatal("empty rates must fail")
	}
	if _, err := ParseRatesResponse([]byte(`not json`), "USD", asOf, "t"); err == nil {
		t.Fatal("invalid json must fail")
	}
}

func TestRateValidate(t *testing.T) {
	if err := (Rate{Base: "USD", Quote: "EUR", Rate: 0.9, Source: "s"}).validate(); err != nil {
		t.Fatal(err)
	}
	for _, r := range []Rate{
		{Quote: "EUR", Rate: 1, Source: "s"},
		{Base: "USD", Rate: 1, Source: "s"},
		{Base: "USD", Quote: "EUR", Rate: -1, Source: "s"},
		{Base: "USD", Quote: "EUR", Rate: 1},
	} {
		if err := r.validate(); err == nil {
			t.Fatalf("must fail: %+v", r)
		}
	}
}
