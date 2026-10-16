package httpx

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A probe the service's own Healthcheck sends to its bind IP: peer == the connection's local address passes, a
// forwarding header or another peer does not.
func TestHealthBindIP(t *testing.T) {
	mux := http.NewServeMux()
	Health(mux, slog.New(slog.DiscardHandler))
	local := &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 8080}
	for _, tc := range []struct {
		name, remote, header string
		local                net.Addr
		want                 int
	}{
		{"same IP", "10.0.0.5:40000", "", local, 200},
		{"same IP, mapped", "[::ffff:10.0.0.5]:40000", "", local, 200},
		{"same IP, v6", "[2001:db8::5]:40000", "", &net.TCPAddr{IP: net.ParseIP("2001:db8::5"), Port: 8080}, 200},
		{"same IP plus XFF", "10.0.0.5:40000", "X-Forwarded-For", local, 404},
		{"same IP plus Forwarded", "10.0.0.5:40000", "Forwarded", local, 404},
		{"other peer", "10.0.0.6:40000", "", local, 404},
		{"no local addr", "10.0.0.5:40000", "", nil, 404},
		{"unix socket", "10.0.0.5:40000", "", &net.UnixAddr{Name: "/run/app.sock", Net: "unix"}, 404},
		{"link-local, other zone", "[fe80::1%eth1]:40000", "", &net.TCPAddr{IP: net.ParseIP("fe80::1"), Zone: "eth0"}, 404},
	} {
		for _, path := range []string{"/healthz", "/readyz"} {
			ctx := context.Background()
			if tc.local != nil {
				ctx = context.WithValue(ctx, http.LocalAddrContextKey, tc.local)
			}
			r := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
			r.RemoteAddr = tc.remote
			if tc.header != "" {
				r.Header.Set(tc.header, "")
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)
			if rec.Code != tc.want {
				t.Errorf("%s %s = %d, want %d", tc.name, path, rec.Code, tc.want)
			}
		}
	}
}

// End to end on a non-loopback address of this machine, as LISTEN_ADDR=<ip>:port binds it.
func TestHealthBindIPOverTCP(t *testing.T) {
	ln := listenNonLoopback(t)
	mux := http.NewServeMux()
	Health(mux, slog.New(slog.DiscardHandler))
	app := httptest.NewUnstartedServer(mux)
	_ = app.Listener.Close()
	app.Listener = ln
	app.Start()
	defer app.Close()
	if got := Healthcheck(LocalURL(ln.Addr().String(), "/readyz")); got != 0 {
		t.Errorf("Healthcheck on the bind IP = %d, want 0", got)
	}
	req, _ := http.NewRequest(http.MethodGet, app.URL+"/healthz", nil)
	req.Header.Set("X-Forwarded-For", "192.0.2.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("bind IP plus X-Forwarded-For = %d, want 404", resp.StatusCode)
	}
}

// listenNonLoopback binds a free port on the first non-loopback, non-link-local IPv4 address of an up interface;
// it skips when there is none or the bind fails.
func listenNonLoopback(t *testing.T) net.Listener {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("no interfaces: %v", err)
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			ln, err := net.Listen("tcp4", net.JoinHostPort(ipn.IP.String(), "0"))
			if err == nil {
				return ln
			}
		}
	}
	t.Skip("no non-loopback IPv4 address to bind")
	return nil
}
