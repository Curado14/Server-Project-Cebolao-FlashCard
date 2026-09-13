package orchestrator

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAccessLogRecordsRequestWithoutSecrets(t *testing.T) {
	var output bytes.Buffer
	logger, err := NewJSONLogger(&output, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	handler := AccessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusUnprocessableEntity, problem{"about:blank", "photo rejected", http.StatusUnprocessableEntity, "photo_rejected"})
	}), logger)

	r := httptest.NewRequest(http.MethodPost, "/api/v1/photos", strings.NewReader("private-photo-body"))
	r.RemoteAddr = "192.168.15.20:54321"
	r.Header.Set("Authorization", "Bearer private-token")
	r.Header.Set("Idempotency-Key", "private-idempotency-key")
	r.Header.Set("User-Agent", "iPhone Shortcuts")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	entry := output.String()
	lines := strings.Split(strings.TrimSpace(entry), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected one completed request log line, got %d: %q", len(lines), entry)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("log entry is not JSON: %v\n%s", err, entry)
	}
	want := map[string]any{
		"event":       "http.request_completed",
		"service":     serviceName,
		"version":     effectiveVersion(),
		"component":   "http",
		"method":      "POST",
		"path":        "/api/v1/photos",
		"status_code": float64(422),
		"error_code":  "photo_rejected",
	}
	for key, expected := range want {
		if got[key] != expected {
			t.Errorf("log field %s=%v, want %v in %q", key, got[key], expected, entry)
		}
	}
	if _, ok := got["request_id"].(string); !ok {
		t.Fatalf("log entry does not contain a request_id: %q", entry)
	}
	for _, secret := range []string{"private-photo-body", "private-token", "private-idempotency-key"} {
		if strings.Contains(entry, secret) {
			t.Errorf("log entry exposed secret %q", secret)
		}
	}
}
