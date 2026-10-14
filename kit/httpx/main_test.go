package httpx

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mainFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// liveHealthz is a server whose /healthz answers 200, and its address.
func liveHealthz(t *testing.T) string {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(up.Close)
	return strings.TrimPrefix(up.URL, "http://")
}

func TestMain_whenHealthcheck_thenProbesWithoutBuilding(t *testing.T) {
	build := func(context.Context) (*http.Server, func(), error) {
		t.Fatal("Build ran for -healthcheck")
		return nil, nil, nil
	}
	if code := Main(context.Background(), mainFlags(), []string{"-healthcheck"}, MainOptions{Addr: liveHealthz(t), Build: build}); code != 0 {
		t.Errorf("Main -healthcheck on a live server = %d, want 0", code)
	}
	if code := Main(context.Background(), mainFlags(), []string{"-healthcheck"}, MainOptions{Addr: freeAddr(t), Build: build}); code != 1 {
		t.Errorf("Main -healthcheck on nothing = %d, want 1", code)
	}
}

func TestMain_whenHealthcheckFlagDefined_thenReusesIt(t *testing.T) {
	build := func(context.Context) (*http.Server, func(), error) {
		t.Fatal("Build ran for -healthcheck")
		return nil, nil, nil
	}
	fs := mainFlags()
	fs.Bool("healthcheck", false, "the program's own")
	if code := Main(context.Background(), fs, []string{"-healthcheck"}, MainOptions{Addr: liveHealthz(t), Build: build}); code != 0 {
		t.Errorf("Main -healthcheck with the flag already on fs = %d, want 0", code)
	}
	fs = mainFlags()
	fs.String("healthcheck", "", "not a bool")
	var buf bytes.Buffer
	o := MainOptions{Addr: "127.0.0.1:1", Build: build, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	if code := Main(context.Background(), fs, nil, o); code != 1 {
		t.Errorf("Main with a string -healthcheck = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "define -healthcheck with fs.Bool, or leave it to Main") {
		t.Errorf("log = %q, want why", buf.String())
	}
}

func TestMain_whenBadCommandLine_thenTwo(t *testing.T) {
	if code := Main(context.Background(), mainFlags(), []string{"-nope"}, MainOptions{}); code != 2 {
		t.Errorf("Main -nope = %d, want 2", code)
	}
	if code := Main(context.Background(), mainFlags(), []string{"-h"}, MainOptions{}); code != 0 {
		t.Errorf("Main -h = %d, want 0", code)
	}
}

func TestMain_whenNoAddr_thenOne(t *testing.T) {
	build := func(context.Context) (*http.Server, func(), error) {
		t.Fatal("Build ran without an Addr")
		return nil, nil, nil
	}
	for _, args := range [][]string{nil, {"-healthcheck"}} {
		var buf bytes.Buffer
		o := MainOptions{Build: build, Log: slog.New(slog.NewTextHandler(&buf, nil))}
		if code := Main(context.Background(), mainFlags(), args, o); code != 1 {
			t.Errorf("Main %v without an Addr = %d, want 1", args, code)
		}
		if !strings.Contains(buf.String(), "Addr is empty") {
			t.Errorf("log = %q, want why", buf.String())
		}
	}
}

func TestMain_whenNoBuildOrNoServer_thenOne(t *testing.T) {
	if code := Main(context.Background(), mainFlags(), nil, MainOptions{Addr: "127.0.0.1:1", Log: quietLog()}); code != 1 {
		t.Errorf("Main without Build = %d, want 1", code)
	}
	cleaned := false
	build := func(context.Context) (*http.Server, func(), error) { return nil, func() { cleaned = true }, nil }
	if code := Main(context.Background(), mainFlags(), nil, MainOptions{Addr: "127.0.0.1:1", Build: build, Log: quietLog()}); code != 1 {
		t.Errorf("Main on a nil server = %d, want 1", code)
	}
	if !cleaned {
		t.Error("cleanup did not run")
	}
}

func TestMain_whenBuildFails_thenOneAndCleanup(t *testing.T) {
	var buf bytes.Buffer
	cleaned := false
	build := func(context.Context) (*http.Server, func(), error) {
		return nil, func() { cleaned = true }, errors.New("no DATABASE_URL")
	}
	o := MainOptions{Addr: "127.0.0.1:1", Build: build, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	if code := Main(context.Background(), mainFlags(), nil, o); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "no DATABASE_URL") {
		t.Errorf("log = %q, want the build error", buf.String())
	}
	if !cleaned {
		t.Error("cleanup did not run")
	}
}

func TestMain_whenBindFails_thenOneAndCleanup(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	cleaned := false
	build := func(context.Context) (*http.Server, func(), error) {
		return NewServer(l.Addr().String(), http.NotFoundHandler()), func() { cleaned = true }, nil
	}
	o := MainOptions{Addr: l.Addr().String(), Build: build, Log: quietLog()}
	if code := Main(context.Background(), mainFlags(), nil, o); code != 1 {
		t.Errorf("Main on a taken port = %d, want 1", code)
	}
	if !cleaned {
		t.Error("cleanup did not run")
	}
}

func TestMain_whenStopped_thenZeroAndCleanup(t *testing.T) {
	addr := freeAddr(t)
	cleaned := make(chan struct{})
	build := func(context.Context) (*http.Server, func(), error) {
		return NewServer(addr, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})), func() { close(cleaned) }, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- Main(ctx, mainFlags(), nil, MainOptions{Addr: addr, Build: build}) }()
	waitUp(t, "http://"+addr+"/x")
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0 on a clean stop", code)
	}
	select {
	case <-cleaned:
	default:
		t.Error("cleanup did not run")
	}
}

// slowStop runs Main on a server whose /slow holds for hold or until its request's context ends, and stops it with
// /slow in flight. It returns the exit code, the log, and whether /slow had returned by the time the cleanup ran.
func slowStop(t *testing.T, drain, hold time.Duration) (code int, log string, handlerDone bool) {
	t.Helper()
	addr := freeAddr(t)
	var once sync.Once
	entered, returned := make(chan struct{}), make(chan struct{})
	build := func(context.Context) (*http.Server, func(), error) {
		srv := NewServer(addr, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/slow" {
				return
			}
			once.Do(func() { close(entered) })
			defer close(returned)
			select {
			case <-r.Context().Done():
			case <-time.After(hold):
			}
		}))
		// The handler wakes on the close asynchronously: give it a moment, not the hold.
		cleanup := func() {
			select {
			case <-returned:
				handlerDone = true
			case <-time.After(5 * time.Second):
			}
		}
		return srv, cleanup, nil
	}
	var buf bytes.Buffer
	o := MainOptions{Addr: addr, Build: build, Drain: drain, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- Main(ctx, mainFlags(), nil, o) }()
	waitUp(t, "http://"+addr+"/")
	go func() {
		resp, err := http.Get("http://" + addr + "/slow") //nolint:noctx // ends with the server
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	cancel()
	code = <-done
	return code, buf.String(), handlerDone
}

func TestMain_whenDrainOverruns_thenCutsOffBeforeCleanupAndZero(t *testing.T) {
	code, log, handlerDone := slowStop(t, 50*time.Millisecond, time.Minute)
	if code != 0 {
		t.Errorf("Main = %d, want 0 when the drain overruns", code)
	}
	if !strings.Contains(log, "drain exceeded") {
		t.Errorf("log = %q, want the overrun warning", log)
	}
	if !handlerDone {
		t.Error("cleanup ran with the handler still in flight: the server was not closed")
	}
}

func TestMain_whenDrainNegative_thenDefault(t *testing.T) {
	code, log, handlerDone := slowStop(t, -time.Second, 200*time.Millisecond)
	if code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}
	if strings.Contains(log, "drain exceeded") || !handlerDone {
		t.Errorf("log = %q, want a drain of DefaultDrain that waits for the handler", log)
	}
}

// getterFlag is a bool flag.Value whose String is not "true" when set: Main must ask Get.
type getterFlag struct{ on bool }

func (f *getterFlag) String() string {
	if f.on {
		return "on"
	}
	return "off"
}
func (f *getterFlag) Set(s string) error { f.on = s == "true"; return nil }
func (f *getterFlag) IsBoolFlag() bool   { return true }
func (f *getterFlag) Get() any           { return f.on }

// plainBoolFlag is a bool flag.Value without Get: its String need not say whether it is set, Main refuses it.
type plainBoolFlag struct{ on bool }

func (f *plainBoolFlag) String() string {
	if f.on {
		return "1"
	}
	return "0"
}
func (f *plainBoolFlag) Set(s string) error { f.on = s == "true"; return nil }
func (f *plainBoolFlag) IsBoolFlag() bool   { return true }

// getterNotBool is a flag.Getter whose Get is no bool.
type getterNotBool struct{ getterFlag }

func (f *getterNotBool) Get() any { return f.String() }

func TestMain_whenHealthcheckFlagCustom_thenReadsOnlyABoolGetter(t *testing.T) {
	build := func(context.Context) (*http.Server, func(), error) {
		t.Fatal("Build ran")
		return nil, nil, nil
	}
	fs := mainFlags()
	fs.Var(&getterFlag{}, "healthcheck", "the program's own")
	if code := Main(context.Background(), fs, []string{"-healthcheck"}, MainOptions{Addr: liveHealthz(t), Build: build}); code != 0 {
		t.Errorf("Main -healthcheck with a bool flag.Getter = %d, want 0 (the probe ran)", code)
	}
	for name, define := range map[string]func(fs *flag.FlagSet){
		"no Getter":       func(fs *flag.FlagSet) { fs.Var(&plainBoolFlag{}, "healthcheck", "") },
		"BoolFunc":        func(fs *flag.FlagSet) { fs.BoolFunc("healthcheck", "", func(string) error { return nil }) },
		"Getter, no bool": func(fs *flag.FlagSet) { fs.Var(&getterNotBool{}, "healthcheck", "") },
	} {
		fs := mainFlags()
		define(fs)
		var buf bytes.Buffer
		o := MainOptions{Addr: liveHealthz(t), Build: build, Log: slog.New(slog.NewTextHandler(&buf, nil))}
		if code := Main(context.Background(), fs, []string{"-healthcheck"}, o); code != 1 {
			t.Errorf("%s: Main -healthcheck = %d, want 1", name, code)
		}
		if !strings.Contains(buf.String(), "define -healthcheck with fs.Bool, or leave it to Main") {
			t.Errorf("%s: log = %q, want why", name, buf.String())
		}
	}
}

func TestMain_whenHealthcheckOnNamedPort_thenProbesItsNumber(t *testing.T) {
	var probed string
	orig := probe
	probe = func(url string) int { probed = url; return 0 }
	t.Cleanup(func() { probe = orig })
	build := func(context.Context) (*http.Server, func(), error) {
		t.Fatal("Build ran for -healthcheck")
		return nil, nil, nil
	}
	if code := Main(context.Background(), mainFlags(), []string{"-healthcheck"}, MainOptions{Addr: ":http", Build: build}); code != 0 {
		t.Errorf("Main -healthcheck = %d, want the probe's 0", code)
	}
	if want := "http://127.0.0.1:80/healthz"; probed != want {
		t.Errorf("probed %q, want %q", probed, want)
	}
}

func TestMain_whenServerAddrEmpty_thenListensOnAddr(t *testing.T) {
	addr := freeAddr(t)
	build := func(context.Context) (*http.Server, func(), error) {
		return NewServer("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})), nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- Main(ctx, mainFlags(), nil, MainOptions{Addr: addr, Build: build, Log: quietLog()}) }()
	waitUp(t, "http://"+addr+"/")
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}
}

func TestMain_whenServerAddrDiffers_thenOneWithoutServing(t *testing.T) {
	addr, other := freeAddr(t), freeAddr(t)
	cleaned := false
	build := func(context.Context) (*http.Server, func(), error) {
		return NewServer(other, http.NotFoundHandler()), func() { cleaned = true }, nil
	}
	var buf bytes.Buffer
	o := MainOptions{Addr: addr, Build: build, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	if code := Main(context.Background(), mainFlags(), nil, o); code != 1 {
		t.Errorf("Main with a server on %s, Addr %s = %d, want 1", other, addr, code)
	}
	if log := buf.String(); !strings.Contains(log, "is not MainOptions.Addr") || !strings.Contains(log, other) || !strings.Contains(log, addr) {
		t.Errorf("log = %q, want why", buf.String())
	}
	if !cleaned {
		t.Error("cleanup did not run")
	}
}

// stopDuringBuild runs Main with a Build that waits for the stop and then returns what ret makes of its ctx.
func stopDuringBuild(t *testing.T, ret func(ctx context.Context) (*http.Server, error)) (code int, log string, cleaned bool) {
	t.Helper()
	entered := make(chan struct{})
	build := func(ctx context.Context) (*http.Server, func(), error) {
		close(entered)
		<-ctx.Done()
		srv, err := ret(ctx)
		return srv, func() { cleaned = true }, err
	}
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, mainFlags(), nil, MainOptions{Addr: "127.0.0.1:1", Build: build, Log: slog.New(slog.NewTextHandler(&buf, nil))})
	}()
	<-entered
	cancel()
	code = <-done
	return code, buf.String(), cleaned
}

func TestMain_whenStopDuringBuild_thenZeroWhateverBuildReturns(t *testing.T) {
	orig := listen
	listen = func(network, addr string) (net.Listener, error) {
		t.Errorf("Main bound %s after a stop during Build", addr)
		return orig(network, addr)
	}
	t.Cleanup(func() { listen = orig })
	for _, tt := range []struct {
		name string
		ret  func(ctx context.Context) (*http.Server, error)
		warn string // in the log at WARN, "" for none
	}{
		{"no server", func(context.Context) (*http.Server, error) { return nil, nil }, ""},
		// The stop's error flattened by a driver: no context.Canceled left to find.
		{"flattened error", func(context.Context) (*http.Server, error) {
			return nil, errors.New("connect: operation was canceled")
		}, "operation was canceled"},
		// A Build that ignores ctx, on an Addr that is not MainOptions.Addr: never bound, never checked.
		{"mismatched Addr", func(context.Context) (*http.Server, error) {
			return NewServer("10.0.0.5:2", http.NotFoundHandler()), nil
		}, ""},
	} {
		code, log, cleaned := stopDuringBuild(t, tt.ret)
		if code != 0 {
			t.Errorf("%s: Main = %d, want 0", tt.name, code)
		}
		if !strings.Contains(log, "Stopped during startup") || strings.Contains(log, "Startup failed") {
			t.Errorf("%s: log = %q, want the stop", tt.name, log)
		}
		if tt.warn != "" && (!strings.Contains(log, "level=WARN") || !strings.Contains(log, tt.warn)) {
			t.Errorf("%s: log = %q, want Build's error at WARN", tt.name, log)
		}
		if !cleaned {
			t.Errorf("%s: cleanup did not run", tt.name)
		}
	}
}

func TestMain_whenAdminHandlerShutsDown_thenCleanupAfterTheHandlers(t *testing.T) {
	addr := freeAddr(t)
	var srv *http.Server
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	stopReturned := make(chan struct{})
	handlersDone := make(chan bool, 1)
	build := func(context.Context) (*http.Server, func(), error) {
		srv = NewServer(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/hold": // a Shutdown leaves r.Context() alone: only release ends it
				close(entered)
				defer close(returned)
				<-release
				return
			case "/stop":
			default:
				return
			}
			defer close(stopReturned)
			go func() { _ = srv.Shutdown(context.Background()) }()
			time.Sleep(200 * time.Millisecond) // answers while the Shutdown is under way
			_, _ = io.WriteString(w, "stopping")
		}))
		var hooked atomic.Int32
		srv.RegisterOnShutdown(func() {
			if hooked.Add(1) > 1 {
				panic("RegisterOnShutdown hook ran twice: a second Shutdown")
			}
		})
		cleanup := func() {
			select {
			case <-stopReturned:
			default:
				handlersDone <- false
				return
			}
			select {
			case <-returned:
				handlersDone <- true
			default:
				handlersDone <- false
			}
		}
		return srv, cleanup, nil
	}
	done := make(chan int, 1)
	go func() {
		done <- Main(context.Background(), mainFlags(), nil, MainOptions{Addr: addr, Build: build, Drain: 5 * time.Second, Log: quietLog()})
	}()
	waitUp(t, "http://"+addr+"/")
	getHold(addr) // another handler in flight
	<-entered
	go func() {
		resp, err := http.Get("http://" + addr + "/stop") //nolint:noctx // the test's own server
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-stopReturned
	select {
	case code := <-done:
		t.Fatalf("Main = %d with /hold in flight: it ran ahead of the handlers", code)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("Main = %d, want 0 on the program's own Shutdown", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Main hung after the Shutdown")
	}
	if !<-handlersDone {
		t.Error("cleanup ran with a handler still in flight")
	}
}

func TestMain_whenStopped_thenBackgroundWorkOnBuildsCtxSeesIt(t *testing.T) {
	addr := freeAddr(t)
	workerStopped := make(chan struct{})
	build := func(ctx context.Context) (*http.Server, func(), error) {
		go func() { <-ctx.Done(); close(workerStopped) }() // background work, as on a scheduler
		return NewServer(addr, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})), nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- Main(ctx, mainFlags(), nil, MainOptions{Addr: addr, Build: build, Log: quietLog()}) }()
	waitUp(t, "http://"+addr+"/")
	cancel()
	select {
	case <-workerStopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Build's ctx did not end at the stop")
	}
	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}
}

func TestMain_whenHijackedHandlerRuns_thenCleanupWaitsForIt(t *testing.T) {
	addr := freeAddr(t)
	entered := make(chan struct{})
	var returned atomic.Bool
	handlerDone := make(chan bool, 1)
	build := func(context.Context) (*http.Server, func(), error) {
		srv := NewServer(addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ws" {
				return
			}
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			close(entered)
			// Shutdown does not wait for a hijacked connection: only the count holds the cleanup back.
			time.Sleep(300 * time.Millisecond)
			returned.Store(true)
		}))
		return srv, func() { handlerDone <- returned.Load() }, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, mainFlags(), nil, MainOptions{Addr: addr, Build: build, Drain: 5 * time.Second, Log: quietLog()})
	}()
	waitUp(t, "http://"+addr+"/")
	go func() {
		resp, err := http.Get("http://" + addr + "/ws") //nolint:noctx // the hijacker closes it
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0", code)
	}
	if !<-handlerDone {
		t.Error("cleanup ran under the hijacked handler")
	}
}

func TestMain_whenHandlerStuck_thenWarnsAndCleansUp(t *testing.T) {
	addr := freeAddr(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	cleaned := false
	build := func(context.Context) (*http.Server, func(), error) {
		srv := NewServer(addr, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/stuck" {
				return
			}
			close(entered)
			<-release // deaf to its request's context
		}))
		return srv, func() { cleaned = true }, nil
	}
	var buf bytes.Buffer
	o := MainOptions{Addr: addr, Build: build, Drain: 50 * time.Millisecond, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- Main(ctx, mainFlags(), nil, o) }()
	waitUp(t, "http://"+addr+"/")
	go func() {
		resp, err := http.Get("http://" + addr + "/stuck") //nolint:noctx // ends with the server
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("Main = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Main hung on a stuck handler")
	}
	if !strings.Contains(buf.String(), "Handlers still running") || !strings.Contains(buf.String(), "handlers=1") {
		t.Errorf("log = %q, want the warning with the count", buf.String())
	}
	if !cleaned {
		t.Error("cleanup did not run")
	}
}

func TestMain_whenDrainOverrunsOnAStuckHandler_thenStopTakesOneBudget(t *testing.T) {
	// A short grace, so one budget (drain+grace, 1.05s) and two (2*drain, the wait taking its own Drain: 2s) lie far
	// apart: the bound leaves 650ms for a slow runner and fails 300ms short of two budgets.
	const drain, grace = time.Second, 50 * time.Millisecond
	orig := handlerGrace
	handlerGrace = grace
	defer func() { handlerGrace = orig }()
	addr := freeAddr(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	build := func(context.Context) (*http.Server, func(), error) {
		srv := NewServer(addr, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/stuck" {
				return
			}
			close(entered)
			<-release // deaf to its request's context: drain and wait both run out
		}))
		return srv, nil, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- Main(ctx, mainFlags(), nil, MainOptions{Addr: addr, Build: build, Drain: drain, Log: quietLog()})
	}()
	waitUp(t, "http://"+addr+"/")
	go func() {
		resp, err := http.Get("http://" + addr + "/stuck") //nolint:noctx // ends with the server
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Main hung on a stuck handler")
	}
	// The drain takes the whole budget; the handler wait gets only the grace on top, not another Drain.
	if took, limit := time.Since(start), 2*drain-300*time.Millisecond; took < drain || took > limit {
		t.Errorf("stop took %v, want about Drain+handlerGrace (%v), under %v", took, drain+grace, limit)
	}
}

// failingServer runs Main on a server whose accept loop fails with /hold in flight. It returns the exit code, the
// log, and whether /hold had returned by the time the cleanup ran.
func failingServer(t *testing.T) (code int, log string, handlerDone bool) {
	t.Helper()
	fail := useFaultyListener(t, nil)
	addr := freeAddr(t)
	h, entered, returned := holdHandler()
	build := func(context.Context) (*http.Server, func(), error) {
		cleanup := func() {
			select {
			case <-returned:
				handlerDone = true
			default:
			}
		}
		return NewServer(addr, h), cleanup, nil
	}
	var buf bytes.Buffer
	o := MainOptions{Addr: addr, Build: build, Drain: time.Minute, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	done := make(chan int, 1)
	go func() { done <- Main(context.Background(), mainFlags(), nil, o) }()
	waitUp(t, "http://"+addr+"/")
	getHold(addr)
	<-entered
	close(fail)
	select {
	case code = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Main hung after the server failed: the handler in flight was not cut")
	}
	return code, buf.String(), handlerDone
}

func TestMain_whenServerFailsOnItsOwn_thenOneAfterHandlersCut(t *testing.T) {
	code, log, handlerDone := failingServer(t)
	if code != 1 {
		t.Errorf("Main = %d, want 1 when the server fails", code)
	}
	if !strings.Contains(log, "Server failed") || !strings.Contains(log, "accept broke") {
		t.Errorf("log = %q, want the failure", log)
	}
	if !handlerDone {
		t.Error("cleanup ran with the handler still in flight")
	}
}

func TestMain_whenListenerCloseFails_thenWarnsAndZero(t *testing.T) {
	useFaultyListener(t, errors.New("close broke"))
	addr := freeAddr(t)
	build := func(context.Context) (*http.Server, func(), error) {
		return NewServer(addr, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})), nil, nil
	}
	var buf bytes.Buffer
	o := MainOptions{Addr: addr, Build: build, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- Main(ctx, mainFlags(), nil, o) }()
	waitUp(t, "http://"+addr+"/")
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0: the stop happened, only the listener's close failed", code)
	}
	if log := buf.String(); !strings.Contains(log, "Shutdown failed") || !strings.Contains(log, "close broke") ||
		strings.Contains(log, "drain exceeded") {
		t.Errorf("log = %q, want the close error, not an overrun", log)
	}
}

func TestSplitAddr(t *testing.T) {
	for _, tt := range []struct {
		addr, host string
		port       int
		err        bool
	}{
		{":8080", "", 8080, false},
		{"0.0.0.0:8080", "", 8080, false},
		{"[::]:8080", "", 8080, false},
		{"[::0]:8080", "", 8080, false},
		{"[0:0:0:0:0:0:0:0]:8080", "", 8080, false},
		{"LocalHost:8080", "localhost", 8080, false},
		{"[0:0::1]:8080", "::1", 8080, false},
		{"[FD00::5]:8080", "fd00::5", 8080, false},
		{":http", "", 80, false},
		{":08080", "", 8080, false},
		{"8080", "", 0, true},
		{":nosuch", "", 0, true},
	} {
		host, port, err := splitAddr(tt.addr)
		if host != tt.host || port != tt.port || (err != nil) != tt.err {
			t.Errorf("splitAddr(%q) = %q, %d, %v; want %q, %d, error %v", tt.addr, host, port, err, tt.host, tt.port, tt.err)
		}
	}
}

func TestMain_whenServerAddrEqualsAfterNormalizing_thenServesElseOne(t *testing.T) {
	addr := freeAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	for _, tt := range []struct {
		srvAddr string
		want    int
	}{
		{"127.0.0.1:" + port, 0},
		{"127.0.0.1:0" + port, 0}, // the port by number
		{"[::ffff:127.0.0.1]:" + port, 1},
		{"localhost:" + port, 1}, // the same machine, not the same Addr
		{"127.0.0.1:1", 1},
	} {
		build := func(context.Context) (*http.Server, func(), error) {
			return NewServer(tt.srvAddr, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})), nil, nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() { done <- Main(ctx, mainFlags(), nil, MainOptions{Addr: addr, Build: build, Log: quietLog()}) }()
		if tt.want == 0 {
			waitUp(t, "http://"+addr+"/")
			cancel()
		}
		code := <-done
		cancel()
		if code != tt.want {
			t.Errorf("Main with the server on %s, Addr %s = %d, want %d", tt.srvAddr, addr, code, tt.want)
		}
	}
}

func TestMain_whenAddrNotLoopbackOrAll_thenOne(t *testing.T) {
	build := func(context.Context) (*http.Server, func(), error) {
		t.Fatal("Build ran on an Addr no probe reaches")
		return nil, nil, nil
	}
	for _, tt := range []struct {
		addr string
		ok   bool
	}{
		{":8080", true},
		{"0.0.0.0:8080", true},
		{"[::]:8080", true},
		{"localhost:8080", true},
		{"LOCALHOST:8080", true},
		{"127.0.0.1:8080", true},
		{"127.0.0.2:8080", true},
		{"[::1]:8080", true},
		{"[::ffff:127.0.0.1]:8080", true},
		{"10.0.0.5:8080", false},
		{"[fd00::5]:8080", false},
		{"app:8080", false},
		{"8080", false},
	} {
		var buf bytes.Buffer
		o := MainOptions{Addr: tt.addr, Build: build, Log: slog.New(slog.NewTextHandler(&buf, nil))}
		orig := probe
		probe = func(string) int { return 0 }
		code := Main(context.Background(), mainFlags(), []string{"-healthcheck"}, o)
		probe = orig
		if want := map[bool]int{true: 0, false: 1}[tt.ok]; code != want {
			t.Errorf("Main -healthcheck on %s = %d, want %d", tt.addr, code, want)
		}
		if !tt.ok && tt.addr != "8080" && !strings.Contains(buf.String(), "must listen on loopback or all interfaces") {
			t.Errorf("%s: log = %q, want why", tt.addr, buf.String())
		}
	}
}
