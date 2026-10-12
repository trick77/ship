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
	"strconv"
	"strings"
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
// csp as the Content-Security-Policy ("" means DefaultCSP). SecurityHeadersWith takes the rest.
func SecurityHeaders(csp string) func(http.Handler) http.Handler {
	return SecurityHeadersWith(SecurityOptions{CSP: csp})
}

// SecurityOptions shapes SecurityHeadersWith. The zero value is SecurityHeaders(""): DefaultCSP and no framing on
// every response.
type SecurityOptions struct {
	// CSP is the Content-Security-Policy, "" means DefaultCSP.
	CSP string
	// HTMLOnly sends CSP, X-Frame-Options and COOP only on text/html responses, judged by the Content-Type the
	// handler set: a JSON or image answer renders nothing a policy could restrict.
	HTMLOnly bool
	// COOP is the Cross-Origin-Opener-Policy ("same-origin"), none when empty.
	COOP string
	// HSTS sends Strict-Transport-Security with this max-age (lens: two years); 0 sends none. Browsers ignore it over
	// plain http, so it is safe behind a TLS-terminating proxy. Opt-in: once sent, a browser refuses http to the
	// host for max-age.
	HSTS time.Duration
	// HSTSSubdomains adds includeSubDomains.
	HSTSSubdomains bool
	// NoIndex marks the paths it matches X-Robots-Tag "noindex, nofollow" (an API prefix, the probes); a handler
	// setting its own X-Robots-Tag replaces it.
	NoIndex func(path string) bool
}

// SecurityHeadersWith sets the headers o describes. HTMLOnly's are added when the handler writes its header, so they
// see the Content-Type it set.
func SecurityHeadersWith(o SecurityOptions) func(http.Handler) http.Handler {
	if o.CSP == "" {
		o.CSP = DefaultCSP
	}
	hsts := ""
	if o.HSTS > 0 {
		hsts = "max-age=" + strconv.FormatInt(int64(o.HSTS/time.Second), 10)
		if o.HSTSSubdomains {
			hsts += "; includeSubDomains"
		}
	}
	html := func(h http.Header) {
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", o.CSP)
		if o.COOP != "" {
			h.Set("Cross-Origin-Opener-Policy", o.COOP)
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			if hsts != "" {
				h.Set("Strict-Transport-Security", hsts)
			}
			// Set up front, so a handler's own (stricter) X-Robots-Tag replaces it.
			if o.NoIndex != nil && o.NoIndex(r.URL.Path) {
				h.Set("X-Robots-Tag", "noindex, nofollow")
			}
			if !o.HTMLOnly {
				html(h)
				next.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(&headerHook{ResponseWriter: w, hook: func(h http.Header) {
				if strings.Contains(h.Get("Content-Type"), "text/html") {
					html(h)
				}
			}}, r)
		})
	}
}

// headerHook runs hook once, just before the header is sent. Unwrap keeps http.ResponseController working.
type headerHook struct {
	http.ResponseWriter
	hook func(http.Header)
	done bool
}

func (w *headerHook) fire() {
	if !w.done {
		w.done = true
		w.hook(w.Header())
	}
}

func (w *headerHook) WriteHeader(code int) {
	w.fire()
	w.ResponseWriter.WriteHeader(code)
}

func (w *headerHook) Write(b []byte) (int, error) {
	w.fire()
	return w.ResponseWriter.Write(b)
}

func (w *headerHook) Unwrap() http.ResponseWriter { return w.ResponseWriter }
