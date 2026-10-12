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

// RequestIDHeader is AccessLog's default request id header.
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

// AccessLogOptions shapes AccessLog. The zero value logs every request under a freshly generated request id.
type AccessLogOptions struct {
	// RequestIDHeader names the header the id is echoed on, and read from when TrustRequestID is set. Empty means
	// RequestIDHeader.
	RequestIDHeader string
	// TrustRequestID adopts a well-formed incoming id, so one id follows a request across hops. Off by default: a
	// header named like a request id can carry a secret (lens's gateway sends a TOTP credential in X-Request-ID), so
	// an untrusted incoming value is never logged, echoed or bound; the request gets its own id instead. Set it only
	// when the proxy in front owns the header.
	TrustRequestID bool
	// NoRequestID turns ids off: the header is neither read nor written, no request_id is logged or bound, and
	// RequestID returns "".
	NoRequestID bool
	// ClientIP resolves the logged ip; nil means ClientIP(r, 0), the TCP peer.
	ClientIP func(*http.Request) string
	// Quiet paths are not logged while they succeed (probes, polled endpoints); a failing one still is.
	Quiet []string
	// Skip drops a request's line, and its OnRecord, when it returns true. Quiet is the common case of it.
	Skip func(r *http.Request, status int) bool
	// Attrs appends fields to the line.
	Attrs func(r *http.Request, status int) []slog.Attr
	// OnRecord runs after every line written, with the ip the line logged (ClientIP resolves it once per request), the
	// final status and the duration: a visitor list, a metric.
	OnRecord func(r *http.Request, ip string, status int, dur time.Duration)
}

// AccessLog writes one "http_request" line per request: request_id, ip, method, path (never the query, which carries
// OIDC codes and tokens), proto, status, duration_ms and user_agent. The id is echoed on the response and bound to
// the request context (logging.With), so every line the handler logs with that context carries it too; see
// AccessLogOptions for where it comes from.
//
// Level follows the status: 5xx error, 4xx warn, else info; 401 stays info, a signed-out browser is not a problem.
func AccessLog(log *slog.Logger, opts AccessLogOptions) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	header := opts.RequestIDHeader
	if header == "" {
		header = RequestIDHeader
	}
	clientIP := opts.ClientIP
	if clientIP == nil {
		clientIP = func(r *http.Request) string { return ClientIP(r, 0) }
	}
	quiet := make(map[string]bool, len(opts.Quiet))
	for _, p := range opts.Quiet {
		quiet[p] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			var id string
			if !opts.NoRequestID {
				if opts.TrustRequestID {
					id = r.Header.Get(header)
				}
				if !requestIDRE.MatchString(id) {
					id = newRequestID()
				}
				w.Header().Set(header, id)
				ctx = context.WithValue(ctx, requestIDKey{}, id)
				ctx = logging.With(ctx, "request_id", id)
			}
			sw := &statusWriter{ResponseWriter: w}
			start := time.Now()
			defer func() {
				dur := time.Since(start)
				// A handler that wrote nothing still answered 200: net/http sends it when the handler returns.
				status := sw.status
				if status == 0 {
					status = http.StatusOK
				}
				if (quiet[r.URL.Path] && status < 400) || (opts.Skip != nil && opts.Skip(r, status)) {
					return
				}
				lvl := slog.LevelInfo
				switch {
				case status >= 500:
					lvl = slog.LevelError
				// 401 (a signed-out browser) and 404 (bots and scanners, most of a public site's traffic) are
				// routine: at warn they would bury the 4xx that point at a real problem.
				case status >= 400 && status != http.StatusUnauthorized && status != http.StatusNotFound:
					lvl = slog.LevelWarn
				}
				ms := float64(dur.Microseconds()) / 1000
				ip := clientIP(r)
				attrs := make([]slog.Attr, 0, 8)
				// The id goes in explicitly, on the unbound context: any handler shows it, none shows it twice.
				if id != "" {
					attrs = append(attrs, slog.String("request_id", id))
				}
				attrs = append(attrs,
					slog.String("ip", ip),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("proto", r.Proto),
					slog.Int("status", status),
					slog.Float64("duration_ms", math.Round(ms*10)/10),
					slog.String("user_agent", r.UserAgent()),
				)
				if opts.Attrs != nil {
					attrs = append(attrs, opts.Attrs(r, status)...)
				}
				log.LogAttrs(r.Context(), lvl, "http_request", attrs...)
				if opts.OnRecord != nil {
					opts.OnRecord(r, ip, status, dur)
				}
			}()
			next.ServeHTTP(sw, r.WithContext(ctx))
		})
	}
}

// Chain wraps h in mws, the first one outermost: Chain(h, a, b) is a(b(h)), so the list reads in request order.
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// Recover turns a handler panic into a logged stack and a 500 (only the log once the handler has written), so one
// bad request never takes the process down.
// http.ErrAbortHandler is re-raised: it is net/http's own signal to drop the connection, not a bug.
// Wrap it inside AccessLog, so the panic's line and the 500 carry the request id.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w}
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
				// Headers already sent: a 500 now would be a superfluous WriteHeader and an error body glued to the
				// handler's output. The log line is all there is.
				if sw.status == 0 {
					WriteError(w, http.StatusInternalServerError, "internal server error")
				}
			}()
			next.ServeHTTP(sw, r)
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
