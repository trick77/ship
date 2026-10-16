package httpx

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"maps"
	"math"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
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

type logAttrsKey struct{}

// lineAttrs collects LogAttrs' fields for one request's access-log line.
type lineAttrs struct {
	mu    sync.Mutex
	attrs []slog.Attr
	done  bool // the line is written
}

// take returns the fields and closes la to further LogAttrs.
func (la *lineAttrs) take() []slog.Attr {
	la.mu.Lock()
	defer la.mu.Unlock()
	la.done = true
	return la.attrs
}

// LogAttrs adds fields to the "http_request" line AccessLog writes for the request ctx belongs to: an account id a
// handler deep in the chain resolved, which AccessLogOptions.Attrs (outside, on the outer request) cannot see. Safe
// from several goroutines; a no-op outside AccessLog, and after the line is written. logging.With, by contrast, puts
// a field on every line the handler logs, not on the access-log line. Keys are not checked: one AccessLog writes
// itself (status, ip, path) duplicates it.
func LogAttrs(ctx context.Context, attrs ...slog.Attr) {
	la, _ := ctx.Value(logAttrsKey{}).(*lineAttrs)
	if la == nil {
		return
	}
	la.mu.Lock()
	defer la.mu.Unlock()
	if !la.done {
		la.attrs = append(la.attrs, attrs...)
	}
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// StatusWriter records the status a handler sent, for a middleware that logs or counts it (AccessLog and Recover
// here; a service's own). Flush and Hijack keep the http.Flusher and http.Hijacker assertions working through it;
// Unwrap keeps the rest of http.ResponseController (deadlines).
type StatusWriter struct {
	http.ResponseWriter
	status int
}

// NewStatusWriter wraps w.
func NewStatusWriter(w http.ResponseWriter) *StatusWriter { return &StatusWriter{ResponseWriter: w} }

// Status is the status sent, 0 before any header or body (net/http sends 200 then, when the handler returns).
func (w *StatusWriter) Status() int { return w.status }

func (w *StatusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *StatusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *StatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// FlushError is what http.ResponseController.Flush calls. A flush sends the header: 200 unless one was written.
func (w *StatusWriter) FlushError() error {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Flush serves the http.Flusher assertion an SSE handler makes behind the access log.
func (w *StatusWriter) Flush() { _ = w.FlushError() }

// Hijack serves the http.Hijacker assertion (websockets).
func (w *StatusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// DefaultProbePaths are the paths Health mounts, AccessLog's default ProbePaths. A func, so no caller can change
// every other caller's default.
func DefaultProbePaths() []string { return []string{"/healthz", "/readyz"} }

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
	// Quiet paths are not logged while they succeed (polled endpoints); a failing one still is.
	Quiet []string
	// ProbePaths are not logged, whatever the status, and run no OnRecord: the orchestrator hits them every few
	// seconds, and Health logs readiness when it changes, so a 503 line per probe is noise. A 404 there is no probe
	// but a caller Health refused, logged like any 404. nil means
	// DefaultProbePaths (where Health mounts them); an app mounting the probes elsewhere names its paths, and an
	// empty non-nil slice logs probes like any path.
	ProbePaths []string
	// Skip drops a request's line, and its OnRecord, when it returns true. Quiet is the common case of it.
	Skip func(r *http.Request, status int) bool
	// Attrs appends fields to the line. It sees the request as AccessLog got it; a handler further in adds its own
	// with LogAttrs, which land after these.
	Attrs func(r *http.Request, status int) []slog.Attr
	// OnRecord runs after every request's line, even one the logger's level drops, with the ip the line logged
	// (ClientIP resolves it once per request), the final status and the duration: a visitor list, a metric.
	OnRecord func(r *http.Request, ip string, status int, dur time.Duration)
	// RoutineLevel is the level of a routine line (StatusLevel); nil means slog.LevelDebug, so the
	// family's default LOG_LEVEL=info hides them and LOG_LEVEL=debug shows them. A service that wants every request
	// at info sets slog.LevelInfo. A Leveler, not a Level, because the zero Level is info. Unused when Level is set.
	RoutineLevel slog.Leveler
	// Level picks a line's level from its status, replacing the whole default rule (StatusLevel); RoutineLevel is
	// then unused. nil means StatusLevel(status, RoutineLevel). A service that moves one status keeps the rest by
	// falling back to StatusLevel: a 409 handshake at debug is
	//
	//	func(s int) slog.Level {
	//		if s == http.StatusConflict {
	//			return slog.LevelDebug
	//		}
	//		return httpx.StatusLevel(s, slog.LevelDebug)
	//	}
	//
	// A routine level that changes at runtime (*slog.LevelVar) is read inside Level.
	Level func(status int) slog.Level
}

// StatusLevel is AccessLog's default level for a status: 5xx error, other 4xx warn, routine lines (status < 400,
// 401 for a signed-out browser, 404 for bots and scanners) routine.
func StatusLevel(status int, routine slog.Level) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	// 401 (a signed-out browser) and 404 (bots and scanners, most of a public site's traffic) are routine: at warn
	// they would bury the 4xx that point at a real problem.
	case status >= 400 && status != http.StatusUnauthorized && status != http.StatusNotFound:
		return slog.LevelWarn
	}
	return routine
}

// AccessLog writes one "http_request" line per request: request_id, ip, method, path (never the query, which carries
// OIDC codes and tokens), proto, status, duration_ms and user_agent. The id is echoed on the response and bound to
// the request context (logging.With), so every line the handler logs with that context carries it too; see
// AccessLogOptions for where it comes from.
//
// Level follows the status (StatusLevel): routine lines log at RoutineLevel, debug by default, so the access log is
// not noise at info. AccessLogOptions.Level replaces the rule. A handler adds its own fields to the line with
// LogAttrs. A service that does not wire AccessLog has no access log at all.
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
	level := opts.Level
	if level == nil {
		var routine slog.Leveler = slog.LevelDebug
		if opts.RoutineLevel != nil {
			routine = opts.RoutineLevel
		}
		// routine.Level() per request: a *slog.LevelVar may change at runtime.
		level = func(status int) slog.Level { return StatusLevel(status, routine.Level()) }
	}
	quiet := make(map[string]bool, len(opts.Quiet))
	for _, p := range opts.Quiet {
		quiet[p] = true
	}
	probePaths := opts.ProbePaths
	if probePaths == nil {
		probePaths = DefaultProbePaths()
	}
	probe := make(map[string]bool, len(probePaths))
	for _, p := range probePaths {
		probe[p] = true
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
			la := &lineAttrs{}
			ctx = context.WithValue(ctx, logAttrsKey{}, la)
			sw := NewStatusWriter(w)
			start := time.Now()
			defer func() {
				dur := time.Since(start)
				extra := la.take()
				// A handler that wrote nothing still answered 200: net/http sends it when the handler returns.
				status := sw.status
				if status == 0 {
					status = http.StatusOK
				}
				if (probe[r.URL.Path] && status != http.StatusNotFound) || (quiet[r.URL.Path] && status < 400) || (opts.Skip != nil && opts.Skip(r, status)) {
					return
				}
				lvl := level(status)
				ms := float64(dur.Microseconds()) / 1000
				ip := clientIP(r)
				attrs := make([]slog.Attr, 0, 8+len(extra))
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
				attrs = append(attrs, extra...)
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
// bad request never takes the process down. The 500 carries the header as it was when Recover's handler started:
// what the outer middleware set (security headers, the request id) stays, what the panicking handler set or changed
// is undone. So is what middleware inside Recover set: one whose header must reach the 500 (CORS) goes outside it.
// http.ErrAbortHandler is re-raised: it is net/http's own signal to drop the connection, not a bug.
// Wrap it inside AccessLog, so the panic's line and the 500 carry the request id.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := NewStatusWriter(w)
			// The header as the outer middleware left it: the 500 must not carry what the handler set before it
			// panicked (a CSV's Content-Disposition, its Cache-Control).
			snap := w.Header().Clone()
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
					h := w.Header()
					clear(h)
					maps.Copy(h, snap)
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
	// HTMLOnly sends CSP, X-Frame-Options and COOP only on active content: text/html, application/xhtml+xml and
	// image/svg+xml (an SVG opened directly runs its script). The type is the Content-Type the handler set, else the
	// one net/http sniffs from the first write; a header sent with no type and no body yet counts as HTML. JSON or
	// a raster image renders nothing a policy could restrict.
	HTMLOnly bool
	// COOP is the Cross-Origin-Opener-Policy ("same-origin"), none when empty.
	COOP string
	// PermissionsPolicy is the Permissions-Policy ("camera=(), microphone=(), geolocation=()"): the browser features
	// the page, and anything it embeds, may never ask for. None when empty; sent with CSP (HTMLOnly applies).
	PermissionsPolicy string
	// HSTS sends Strict-Transport-Security with this max-age (lens: two years); 0 sends none. Browsers ignore it over
	// plain http, so it is safe behind a TLS-terminating proxy. Opt-in: once sent, a browser refuses http to the
	// host for max-age. Whole seconds; a positive value below one second panics (it would be max-age=0).
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
	if o.HSTS > 0 && o.HSTS < time.Second {
		// max-age counts seconds: this would send max-age=0, which tells the browser to forget HSTS.
		panic("httpx.SecurityHeadersWith: HSTS below one second (a unitless number is nanoseconds; use time.Hour*…)")
	}
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
		if o.PermissionsPolicy != "" {
			h.Set("Permissions-Policy", o.PermissionsPolicy)
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
			next.ServeHTTP(&headerHook{ResponseWriter: w, hook: func(h http.Header, body []byte) {
				ct := h.Get("Content-Type")
				switch {
				// net/http sniffs an unset type from the first write; decide on the same bytes.
				case ct == "" && body != nil:
					ct = http.DetectContentType(body)
				// Headers sent with nothing to sniff yet: the type is unknown, so assume a page.
				case ct == "":
					html(h)
					return
				}
				if active(ct) {
					html(h)
				}
			}}, r)
		})
	}
}

// active reports whether a browser runs script in a document of this type: HTML, XHTML and SVG opened directly.
func active(contentType string) bool {
	mt, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(mt)) {
	case "text/html", "application/xhtml+xml", "image/svg+xml":
		return true
	}
	return false
}

// headerHook runs hook once, just before the header is sent, with the first write's bytes when a write sends it.
// Flush sends the header too, so it fires the hook first: http.ResponseController and an http.Flusher assertion
// (server-sent events) both land there. Hijack passes through: the connection leaves HTTP, no header is sent.
// Unwrap keeps the rest of http.ResponseController (deadlines) working.
type headerHook struct {
	http.ResponseWriter
	hook func(h http.Header, body []byte)
	done bool
}

func (w *headerHook) fire(body []byte) {
	if !w.done {
		w.done = true
		w.hook(w.Header(), body)
	}
}

func (w *headerHook) WriteHeader(code int) {
	w.fire(nil)
	w.ResponseWriter.WriteHeader(code)
}

func (w *headerHook) Write(b []byte) (int, error) {
	if b == nil {
		b = []byte{}
	}
	w.fire(b)
	return w.ResponseWriter.Write(b)
}

// FlushError is what http.ResponseController.Flush calls.
func (w *headerHook) FlushError() error {
	w.fire(nil)
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Flush serves the http.Flusher assertion.
func (w *headerHook) Flush() { _ = w.FlushError() }

func (w *headerHook) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

func (w *headerHook) Unwrap() http.ResponseWriter { return w.ResponseWriter }
