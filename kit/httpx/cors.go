package httpx

import (
	"net/http"
	"slices"
	"strings"
)

// CORSOptions shapes CORS. The zero value is a public read API: any origin, GET/POST/HEAD, any request header, no
// credentials.
type CORSOptions struct {
	// Origins are the exact origins allowed ("https://app.example.com"), matched case-insensitively and echoed back.
	// Empty means any origin, answered with "*".
	Origins []string
	// Methods a cross-origin request may use; empty means GET, POST and HEAD. OPTIONS is always allowed.
	Methods []string
	// Headers a preflight may ask for; empty means any. A preflight asking for one outside the list gets no grant.
	Headers []string
	// Credentials lets the browser send cookies and read the response. It needs Origins: a wildcard origin with
	// credentials would hand every site the user's session, so CORS panics on that combination.
	Credentials bool
}

// CORS answers cross-origin requests per opts. A preflight (OPTIONS carrying Origin and
// Access-Control-Request-Method) is answered here with 200 and never reaches next; one that asks for something
// opts do not allow gets the 200 without grants, which the browser reads as a refusal. Every response varies on
// Origin, so a cache never serves one origin's grant to another.
func CORS(opts CORSOptions) func(http.Handler) http.Handler {
	if opts.Credentials && len(opts.Origins) == 0 {
		panic("httpx.CORS: Credentials needs explicit Origins")
	}
	methods := []string{http.MethodGet, http.MethodPost, http.MethodHead}
	if len(opts.Methods) > 0 {
		methods = make([]string, len(opts.Methods))
		for i, m := range opts.Methods {
			methods[i] = strings.ToUpper(m)
		}
	}
	methodOK := func(m string) bool {
		m = strings.ToUpper(m)
		return m == http.MethodOptions || slices.Contains(methods, m)
	}
	originOK := func(o string) bool {
		return len(opts.Origins) == 0 || slices.ContainsFunc(opts.Origins, func(a string) bool { return strings.EqualFold(a, o) })
	}
	allowedHeaders := parseHeaderList(strings.Join(opts.Headers, ","))
	headersOK := func(hs []string) bool {
		if len(opts.Headers) == 0 {
			return true
		}
		for _, h := range hs {
			if !slices.Contains(allowedHeaders, h) {
				return false
			}
		}
		return true
	}
	grant := func(h http.Header, origin string) {
		if len(opts.Origins) == 0 {
			h.Set("Access-Control-Allow-Origin", "*")
		} else {
			h.Set("Access-Control-Allow-Origin", origin)
		}
		if opts.Credentials {
			h.Set("Access-Control-Allow-Credentials", "true")
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			_, hasOrigin := r.Header["Origin"]
			origin := r.Header.Get("Origin")
			reqMethod := r.Header.Get("Access-Control-Request-Method")
			if r.Method == http.MethodOptions && reqMethod != "" && hasOrigin {
				h.Add("Vary", "Origin")
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				reqHeaders := parseHeaderList(r.Header.Get("Access-Control-Request-Headers"))
				if originOK(origin) && methodOK(reqMethod) && headersOK(reqHeaders) {
					grant(h, origin)
					h.Set("Access-Control-Allow-Methods", strings.ToUpper(reqMethod))
					if len(reqHeaders) > 0 {
						h.Set("Access-Control-Allow-Headers", strings.Join(reqHeaders, ", "))
					}
				}
				w.WriteHeader(http.StatusOK)
				return
			}
			h.Add("Vary", "Origin")
			if hasOrigin && originOK(origin) && methodOK(r.Method) {
				grant(h, origin)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// parseHeaderList splits a comma or space separated header list into canonical names ("x-api-key" is
// "X-Api-Key"), dropping bytes outside [A-Za-z0-9._-]: the names are echoed into a response header, so request
// bytes must not reach it raw.
func parseHeaderList(list string) []string {
	var headers []string
	h := make([]byte, 0, len(list))
	upper := true
	for i := 0; i < len(list); i++ {
		b := list[i]
		switch {
		case b >= 'a' && b <= 'z':
			if upper {
				b -= 'a' - 'A'
			}
			h = append(h, b)
		case b >= 'A' && b <= 'Z':
			if !upper {
				b += 'a' - 'A'
			}
			h = append(h, b)
		case b == '-' || b == '_' || b == '.' || (b >= '0' && b <= '9'):
			h = append(h, b)
		}
		if b == ' ' || b == ',' || i == len(list)-1 {
			if len(h) > 0 {
				headers = append(headers, string(h))
				h = h[:0]
				upper = true
			}
		} else {
			upper = b == '-'
		}
	}
	return headers
}
