package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/trick77/ship/kit/logging"
)

// RequestIDHeader carries the request id in and out: a proxy's id is kept so one id follows a request across hops.
const RequestIDHeader = "X-Request-Id"

// requestIDRE bounds a caller-supplied id: untrusted input must not become an indexed log field (newlines, huge
// strings).
var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type requestIDKey struct{}

// RequestID is the id AccessLog assigned to the request ctx belongs to, "" outside one.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// statusWriter records the status a handler sent. Unwrap keeps http.ResponseController working through it, or
// Flush (server-sent events) and deadline control would stop at this wrapper.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// AccessLog writes one "http_request" line per request: method, path (never the query, which carries OIDC codes and
// tokens), status, duration_ms and request_id. The id comes from X-Request-Id when it is well-formed, else is
// generated; it is echoed on the response and bound to the request context (logging.With), so every line the
// handler logs with that context carries it too.
//
// Level follows the status: 5xx error, 4xx warn, else info; 401 stays info, a signed-out browser is not a problem.
// quiet paths (probes, polled endpoints) are not logged while they succeed; a failing probe still is.
func AccessLog(log *slog.Logger, quiet ...string) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	silent := make(map[string]bool, len(quiet))
	for _, p := range quiet {
		silent[p] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if !requestIDRE.MatchString(id) {
				id = newRequestID()
			}
			w.Header().Set(RequestIDHeader, id)
			ctx := context.WithValue(r.Context(), requestIDKey{}, id)
			ctx = logging.With(ctx, "request_id", id)
			sw := &statusWriter{ResponseWriter: w}
			start := time.Now()
			defer func() {
				// A handler that wrote nothing still answered 200: net/http sends it when the handler returns.
				status := sw.status
				if status == 0 {
					status = http.StatusOK
				}
				if silent[r.URL.Path] && status < 400 {
					return
				}
				lvl := slog.LevelInfo
				switch {
				case status >= 500:
					lvl = slog.LevelError
				case status >= 400 && status != http.StatusUnauthorized:
					lvl = slog.LevelWarn
				}
				ms := float64(time.Since(start).Microseconds()) / 1000
				// The id goes in explicitly, on the unbound context: any handler shows it, none shows it twice.
				log.Log(r.Context(), lvl, "http_request", "request_id", id, "method", r.Method, "path", r.URL.Path, "status", status,
					"duration_ms", math.Round(ms*10)/10)
			}()
			next.ServeHTTP(sw, r.WithContext(ctx))
		})
	}
}

// Recover turns a handler panic into a logged stack and a 500, so one bad request never takes the process down.
// http.ErrAbortHandler is re-raised: it is net/http's own signal to drop the connection, not a bug.
// Wrap it inside AccessLog, so the panic's line and the 500 carry the request id.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				log.ErrorContext(r.Context(), "Panic recovered", "panic", rec, "path", r.URL.Path,
					"stack", string(debug.Stack()))
				WriteError(w, http.StatusInternalServerError, "internal server error")
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// CrossOrigin refuses cross-origin browser mutations (Go 1.25's CrossOriginProtection). SameSite=Lax still sends a
// session cookie on a form POST from a sibling subdomain, so the cookie alone is no CSRF defence. A request with
// neither Sec-Fetch-Site nor Origin (curl, an agent) passes: it is not a browser riding a cookie. GET and HEAD are
// never checked, so an OIDC callback is unaffected.
func CrossOrigin(h http.Handler) http.Handler {
	return http.NewCrossOriginProtection().Handler(h)
}

// DefaultCSP suits an app that loads nothing from elsewhere: own scripts and styles, inline styles (Vite injects
// some), data: images, no framing.
const DefaultCSP = "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// SecurityHeaders sets the baseline headers on every response: no sniffing, same-origin referrers, no framing, and
// csp as the Content-Security-Policy ("" means DefaultCSP).
func SecurityHeaders(csp string) func(http.Handler) http.Handler {
	if csp == "" {
		csp = DefaultCSP
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Content-Security-Policy", csp)
			next.ServeHTTP(w, r)
		})
	}
}
