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
	if !strings.Contains(buf.String(), "not a bool") {
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

func TestMain_whenBuildFails_thenOne(t *testing.T) {
	var buf bytes.Buffer
	build := func(context.Context) (*http.Server, func(), error) { return nil, nil, errors.New("no DATABASE_URL") }
	o := MainOptions{Addr: "127.0.0.1:1", Build: build, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	if code := Main(context.Background(), mainFlags(), nil, o); code != 1 {
		t.Errorf("Main = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "no DATABASE_URL") {
		t.Errorf("log = %q, want the build error", buf.String())
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
