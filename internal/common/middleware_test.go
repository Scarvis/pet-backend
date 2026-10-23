package common

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func decodeTestResponse(t *testing.T, w *httptest.ResponseRecorder) (int, map[string]any) {
	t.Helper()
	res := w.Result()
	status := res.StatusCode
	var payload map[string]any
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if err := res.Body.Close(); err != nil {
		t.Fatalf("close response body: %v", err)
	}
	return status, payload
}

func assertActionShape(t *testing.T, payload map[string]any, wantMessage string) {
	t.Helper()
	if _, ok := payload["message"]; !ok {
		t.Fatalf("response %v has no \"message\" key; want the Hasura action error shape", payload)
	}
	if _, ok := payload["error"]; ok {
		t.Fatalf("response %v renders the Events/Cron {\"error\"} shape on the Action path", payload)
	}
	if got, _ := payload["message"].(string); got != wantMessage {
		t.Fatalf("message = %q, want %q", got, wantMessage)
	}
}

// The middleware guards the shared Actions/Events/Cron tree; Hasura Actions
// parse the failure body and require {"message": ...} (§6.4), while Events and
// Cron delivery only read the status code — so the action shape is safe everywhere.
func TestMiddlewareUnauthorizedRendersActionErrorShape(t *testing.T) {
	mw := NewMiddleware(nil, "secret", discardLogger())

	r := httptest.NewRequest(http.MethodPost, "/actions/createTransaction", nil)
	w := httptest.NewRecorder()
	mw.Handler(http.NotFoundHandler()).ServeHTTP(w, r)

	status, payload := decodeTestResponse(t, w)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", status, http.StatusUnauthorized)
	}
	assertActionShape(t, payload, "missing or invalid X-Action-Secret")
}

func TestMiddlewarePanicRendersActionErrorShape(t *testing.T) {
	mw := NewMiddleware(nil, "", discardLogger()) // empty secret disables verification

	next := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Panic-Value") == "typed" {
			panic(BadRequest("field 'amount_minor' must be a decimal string"))
		}
		panic("boom")
	}

	r := httptest.NewRequest(http.MethodPost, "/actions/createTransaction", nil)
	r.Header.Set("X-Panic-Value", "generic")
	w := httptest.NewRecorder()
	mw.Handler(http.HandlerFunc(next)).ServeHTTP(w, r)

	status, payload := decodeTestResponse(t, w)
	if status != http.StatusInternalServerError {
		t.Fatalf("generic panic: status = %d, want %d", status, http.StatusInternalServerError)
	}
	assertActionShape(t, payload, "internal server error")

	r.Header.Set("X-Panic-Value", "typed")
	w = httptest.NewRecorder()
	mw.Handler(http.HandlerFunc(next)).ServeHTTP(w, r)

	status, payload = decodeTestResponse(t, w)
	if status != http.StatusBadRequest {
		t.Fatalf("typed panic: status = %d, want %d", status, http.StatusBadRequest)
	}
	assertActionShape(t, payload, "field 'amount_minor' must be a decimal string")
}
