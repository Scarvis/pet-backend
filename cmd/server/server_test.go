package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Scarvis/pet-backend/internal/common"
)

func TestParseNumeric(t *testing.T) {
	// JSON string form (Hasura numeric as string).
	got, err := parseNumeric(json.RawMessage(`"12.5"`))
	if err != nil || got != "12.5" {
		t.Fatalf("string quantity: got %q err %v", got, err)
	}
	// JSON number form.
	got, err = parseNumeric(json.RawMessage(`12.5`))
	if err != nil || got != "12.5" {
		t.Fatalf("number quantity: got %q err %v", got, err)
	}
	// Missing / null / blank must fail.
	for _, raw := range []string{``, `null`, `"  "`} {
		if _, err := parseNumeric(json.RawMessage(raw)); err == nil {
			t.Fatalf("quantity %q must fail", raw)
		}
	}
	// Non-numeric must fail.
	if _, err := parseNumeric(json.RawMessage(`true`)); err == nil {
		t.Fatal("boolean quantity must fail")
	}
}

func TestToBigint(t *testing.T) {
	if got := toBigint(12345); string(got) != "12345" {
		t.Fatalf("got %q", got)
	}
	if got := toBigint(-7); string(got) != "-7" {
		t.Fatalf("got %q", got)
	}
}

// TestHandleDecodeFailureWritesActionErrorShape pins the Hasura Action error
// contract (§6.4): a webhook failure must render {"message": ...}. The shared
// {"error": {...}} Events/Cron shape makes Hasura fail to parse the error
// response ("key \"message\" not found") and masks the real cause from the
// client.
func TestHandleDecodeFailureWritesActionErrorShape(t *testing.T) {
	type req struct {
		AmountMinor common.Bigint `json:"amount_minor"`
	}
	// amount_minor sent as a JSON number violates the Bigint string wire
	// contract and must surface as the real decode error, not parse-failed.
	body := `{"action":{"name":"createTransaction"},"input":{"amount_minor":5}}`
	r := httptest.NewRequest(http.MethodPost, "/actions/createTransaction", strings.NewReader(body))
	w := httptest.NewRecorder()

	handle[req, int](w, r, func(context.Context, req) (int, error) {
		t.Error("handler must not run for an invalid input")
		return 0, nil
	})

	res := w.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusBadRequest)
	}
	var payload map[string]any
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if _, ok := payload["message"]; !ok {
		t.Fatalf("response %v has no \"message\" key; want the Hasura action error shape", payload)
	}
	if _, ok := payload["error"]; ok {
		t.Fatalf("response %v renders the Events/Cron {\"error\"} shape on the Action path", payload)
	}
	msg, _ := payload["message"].(string)
	if !strings.Contains(msg, "invalid bigint") {
		t.Fatalf("message = %q, want the underlying decode failure", msg)
	}
}
