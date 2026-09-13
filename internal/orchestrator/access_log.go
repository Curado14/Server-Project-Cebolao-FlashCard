package orchestrator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const serviceName = "photo-edge-orchestrator"

var Version = "dev"

type requestIDContextKey struct{}

type accessLogResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *accessLogResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *accessLogResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}

func (w *accessLogResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func NewJSONLogger(out io.Writer, cfg Config) (*slog.Logger, error) {
	return newJSONLogger(out, cfg, nil)
}

func newJSONLogger(out io.Writer, cfg Config, onError func()) (*slog.Logger, error) {
	if out == nil {
		return nil, errors.New("logger output is required")
	}
	handler := slog.NewJSONHandler(out, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				if t, ok := attr.Value.Any().(time.Time); ok {
					attr.Value = slog.StringValue(t.UTC().Format(time.RFC3339Nano))
				}
			}
			return attr
		},
	}).WithAttrs([]slog.Attr{
		slog.String("service", serviceName),
		slog.String("version", effectiveVersion()),
		slog.String("instance_id", cfg.InstanceID),
	})
	if onError != nil {
		return slog.New(loggingErrorHandler{next: handler, onError: onError}), nil
	}
	return slog.New(handler), nil
}

type loggingErrorHandler struct {
	next    slog.Handler
	onError func()
}

func (h loggingErrorHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h loggingErrorHandler) Handle(ctx context.Context, record slog.Record) error {
	err := h.next.Handle(ctx, record)
	if err != nil {
		h.onError()
	}
	return err
}

func (h loggingErrorHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return loggingErrorHandler{next: h.next.WithAttrs(attrs), onError: h.onError}
}

func (h loggingErrorHandler) WithGroup(name string) slog.Handler {
	return loggingErrorHandler{next: h.next.WithGroup(name), onError: h.onError}
}

func effectiveVersion() string {
	if strings.TrimSpace(Version) == "" {
		return "dev"
	}
	return safeIdentifier(Version, "dev")
}

func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDContextKey{}).(string)
	return id
}

func withRequestID(r *http.Request) (*http.Request, string) {
	key := r.Header.Get("Idempotency-Key")
	requestID := r.Header.Get("X-Request-Id")
	if !validRequestID(requestID) || requestID == key {
		requestID = newRequestID()
	}
	ctx := context.WithValue(r.Context(), requestIDContextKey{}, requestID)
	return r.WithContext(ctx), requestID
}

func validRequestID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "req-" + hex.EncodeToString(b[:])
	}
	return id("req")
}

func idempotencyHash(value string) string {
	sum := sha256String(value)
	if len(sum) > 16 {
		return sum[:16]
	}
	return sum
}

func sha256String(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])
}

func AccessLog(next http.Handler, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		r, requestID := withRequestID(r)
		response := &accessLogResponseWriter{ResponseWriter: w}
		response.Header().Set("X-Request-Id", requestID)

		logger.DebugContext(r.Context(), "request started",
			"event", "http.request_started",
			"component", "http",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
		)

		next.ServeHTTP(response, r)

		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		logger.InfoContext(r.Context(), "request completed",
			"event", "http.request_completed",
			"component", "http",
			"request_id", requestID,
			"method", r.Method,
			"path", r.URL.Path,
			"status_code", status,
			"error_code", response.Header().Get("X-PhotoEdge-Problem-Code"),
			"duration_ms", time.Since(started).Milliseconds(),
		)
	})
}
