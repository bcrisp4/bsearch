package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// requestIDBytes is the length of the correlation ID. Long enough that two
// requests in one daemon's lifetime won't collide, short enough to read.
const requestIDBytes = 6

// statusRecorder captures the status code for the request log. Without it the
// log can only say a request happened, not how it ended.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// logRequests records one line per request: method, route, status, duration
// and a correlation ID. Never the query, never results, at any level — a
// debug level that logs queries is one config edit away from a leak, so it
// does not exist (DESIGN.md: Privacy; ADR 0009).
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := requestID()
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		// Echoed so a user reporting a failure can point at the log line.
		w.Header().Set("X-Request-Id", id)

		next.ServeHTTP(rec, r.WithContext(withRequestID(r.Context(), id)))

		s.log.Info("request",
			"id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// recoverPanics turns a panicking handler into a 500 with the envelope.
// net/http recovers per-connection panics already, but the client is left
// with a bare EOF and no way to tell a crash from a network fault.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler { //nolint:errorlint // sentinel panic value, compared by identity per net/http
				panic(v) // deliberate abort; let net/http handle it
			}
			s.log.Error("panic serving request",
				"id", requestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"panic", v,
			)
			writeError(w, s.log, http.StatusInternalServerError, codeInternal, genericInternalMessage)
		}()
		next.ServeHTTP(w, r)
	})
}

// requestID returns a short random correlation ID. Randomness failure is not
// worth failing a request over — an empty ID only costs correlation.
func requestID() string {
	buf := make([]byte, requestIDBytes)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

type requestIDKey struct{}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// newStrictDecoder rejects unknown fields, which is what makes a misspelled
// request field a loud 400 rather than a silently ignored one.
func newStrictDecoder(r io.Reader) *json.Decoder {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	return dec
}
