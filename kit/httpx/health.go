package httpx

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/trick77/ship/kit/logging"
)

// Check is one readiness dependency: a database ping, an upstream that must answer. Name is what the 503 body and
// the log report.
type Check struct {
	Name string
	Run  func(context.Context) error
}

// CheckTimeout bounds one readiness run across all checks: a dependency that hangs must fail the probe, not hold
// it until the orchestrator's own timeout reports something vaguer.
const CheckTimeout = 5 * time.Second

// Health registers the probes on mux.
//
// GET /healthz is liveness: 200 "ok" while the process serves. It checks nothing, because a dependency outage is not
// fixed by a restart, and a liveness probe that fails on one restarts every replica into the same outage.
//
// GET /readyz is readiness: every check in order, 503 at the first failure with the check's name. The error itself
// goes to log.
//
// Both answer only a direct caller on this host: the peer is loopback, or the address the connection arrived on (a
// service bound to one IP, LISTEN_ADDR=10.0.0.5:8080, whose Healthcheck dials that IP), and the request carries no
// X-Forwarded-For, Forwarded or X-Real-IP. Anyone else gets 404, as for a path that does not exist. The probes are
// the orchestrator's, which probes from inside (Healthcheck, an exec probe); nobody else needs to know they exist. A
// proxy in the same network namespace (a sidecar) connects from loopback too, its forwarding header refuses it.
//
// The log reports transitions, not probes: an orchestrator probes every few seconds, so a line per failing probe
// buries the log for as long as a dependency is down. One warn when a check starts failing (its name and error),
// one info when the probe passes again; the same check failing again stays silent, a different one warns anew.
// AccessLog leaves both paths out (AccessLogOptions.ProbePaths): this log is their record.
func Health(mux *http.ServeMux, log *slog.Logger, checks ...Check) {
	if log == nil {
		log = slog.Default()
	}
	rd := &readiness{checks: checks, log: log}
	mux.HandleFunc("GET /healthz", local(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	}))
	mux.HandleFunc("GET /readyz", local(rd.serve))
}

// local serves h to a direct caller on this host only, 404 to anyone else (see Health).
func local(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLocal(r) {
			http.NotFound(w, r)
			return
		}
		h(w, r)
	}
}

// isLocal: the TCP peer is loopback or the connection's own local address, and no proxy forwarded the request. A
// forwarding header only ever refuses, so a caller cannot add one to get in.
//
// Peer == local address is safe: a TCP handshake from that source completes only from this host (a spoofed source
// never sees the SYN-ACK), and another host or a bridged container connects from its own address. The residual: in a
// private network namespace loopback reaches only the container, the bind IP also anything sharing the namespace, so
// a host-network process on the same machine passes too. Such a process proxying to the bind IP must send a
// forwarding header (nginx's proxy_pass adds none by default), as a proxy to loopback always had to; a layer-4
// proxy (HAProxy mode tcp, socat) cannot, and publishes the probes, to loopback and bind IP alike.
func isLocal(r *http.Request) bool {
	for _, h := range []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip"} {
		if _, ok := r.Header[h]; ok {
			return false
		}
	}
	ip, ok := addrIP(r.RemoteAddr)
	if !ok {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	lip, ok := addrIP(local.String())
	return ok && lip == ip
}

// addrIP parses a host:port's host, unmapped. The zone stays: a link-local address matches only on its own link.
func addrIP(hostport string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return netip.Addr{}, false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

// readiness runs the checks for /readyz and remembers the last state for the transition log.
type readiness struct {
	checks []Check
	log    *slog.Logger

	mu      sync.Mutex
	down    bool   // the last logged transition was a failure
	failing string // the check it named
}

func (rd *readiness) serve(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), CheckTimeout)
	defer cancel()
	var (
		check string
		err   error
	)
	for _, c := range rd.checks {
		if err = c.Run(ctx); err != nil {
			check = c.Name
			break
		}
	}
	rd.transition(ctx, check, err)
	if err != nil {
		WriteJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "check": check})
		return
	}
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// transition logs a change of state. A nil err is ready.
func (rd *readiness) transition(ctx context.Context, check string, err error) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	switch {
	case err != nil && (!rd.down || check != rd.failing):
		rd.log.WarnContext(ctx, "Readiness check failed", "check", check, logging.Err(err))
	case err == nil && rd.down:
		rd.log.InfoContext(ctx, "Readiness restored", "check", rd.failing)
	}
	rd.down, rd.failing = err != nil, check
}
