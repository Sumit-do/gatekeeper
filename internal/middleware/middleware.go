// Package middleware provides composable net/http middleware used by the
// gateway: request IDs, structured logging, panic recovery, per-request
// timeouts and response metrics.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/sumit-do/gatekeeper/internal/metrics"
)

// Middleware wraps an http.Handler with additional behaviour.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares to h so that the first middleware in the list is
// the outermost (it sees the request first and the response last).
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// RequestIDHeader carries the per-request correlation ID.
const RequestIDHeader = "X-Request-ID"

type requestIDKey struct{}

// RequestID ensures every request carries an X-Request-ID. An incoming ID is
// preserved; otherwise a random 128-bit hex ID is generated. The ID is
// stored in the request context, set on the forwarded request headers and
// echoed on the response.
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if id == "" {
				id = NewID()
			}
			r = r.Clone(context.WithValue(r.Context(), requestIDKey{}, id))
			r.Header.Set(RequestIDHeader, id)
			w.Header().Set(RequestIDHeader, id)
			next.ServeHTTP(w, r)
		})
	}
}

// RequestIDFromContext returns the request ID stored by RequestID, or "".
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// NewID returns a random 32-character hex string.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is effectively fatal on every supported
		// platform; fall back to a time-based value rather than panicking.
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Logging emits one structured log line per request.
func Logging(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := NewRecorder(w)
			next.ServeHTTP(rec, r)

			level := slog.LevelInfo
			switch {
			case rec.Status() >= 500:
				level = slog.LevelError
			case rec.Status() >= 400:
				level = slog.LevelWarn
			}
			logger.LogAttrs(r.Context(), level, "request",
				slog.String("request_id", RequestIDFromContext(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.Status()),
				slog.Int64("bytes", rec.Bytes()),
				slog.Float64("duration_ms", float64(time.Since(start))/float64(time.Millisecond)),
				slog.String("remote", r.RemoteAddr),
				slog.String("user_agent", r.UserAgent()),
			)
		})
	}
}

// Recover converts panics in downstream handlers into 500 responses and logs
// the stack trace. http.ErrAbortHandler is re-panicked as net/http expects.
func Recover(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := NewRecorder(w)
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				if p == http.ErrAbortHandler {
					panic(p)
				}
				logger.LogAttrs(r.Context(), slog.LevelError, "panic recovered",
					slog.String("request_id", RequestIDFromContext(r.Context())),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("panic", fmt.Sprint(p)),
					slog.String("stack", string(debug.Stack())),
				)
				if !rec.Written() {
					http.Error(rec, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(rec, r)
		})
	}
}

// Timeout attaches a deadline of d to the request context. Handlers are
// expected to observe ctx.Done(); the proxy maps a deadline to 504. A
// non-positive d disables the middleware.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		if d <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Metrics records the status and duration of every request under route.
func Metrics(reg *metrics.Registry, route string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := NewRecorder(w)
			next.ServeHTTP(rec, r)
			reg.ObserveRequest(route, rec.Status(), time.Since(start))
		})
	}
}
