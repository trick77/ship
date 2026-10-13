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
	"testing"
	"time"
)

func mainFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// waitUp polls addr until it answers, or fails the test.
func waitUp(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for healthcheck(url, io.Discard) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("server never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMain_whenHealthcheck_thenProbesWithoutBuilding(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer up.Close()
	build := func(context.Context) (*http.Server, func(), error) {
		t.Fatal("Build ran for -healthcheck")
		return nil, nil, nil
	}
	addr := strings.TrimPrefix(up.URL, "http://")
	if code := Main(context.Background(), mainFlags(), []string{"-healthcheck"}, MainOptions{Addr: addr, Build: build}); code != 0 {
		t.Errorf("Main -healthcheck on a live server = %d, want 0", code)
	}
	if code := Main(context.Background(), mainFlags(), []string{"-healthcheck"}, MainOptions{Addr: freeAddr(t), Build: build}); code != 1 {
		t.Errorf("Main -healthcheck on nothing = %d, want 1", code)
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

func TestMain_whenBuildFails_thenOne(t *testing.T) {
	var buf bytes.Buffer
	build := func(context.Context) (*http.Server, func(), error) { return nil, nil, errors.New("no DATABASE_URL") }
	o := MainOptions{Build: build, Log: slog.New(slog.NewTextHandler(&buf, nil))}
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
	o := MainOptions{Build: build, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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

func TestMain_whenDrainOverruns_thenZeroWithWarning(t *testing.T) {
	addr := freeAddr(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var srv *http.Server
	build := func(context.Context) (*http.Server, func(), error) {
		srv = NewServer(addr, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/slow" {
				close(entered)
				<-release
			}
		}))
		return srv, nil, nil
	}
	var buf bytes.Buffer
	o := MainOptions{Addr: addr, Build: build, Drain: 50 * time.Millisecond, Log: slog.New(slog.NewTextHandler(&buf, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- Main(ctx, mainFlags(), nil, o) }()
	waitUp(t, "http://"+addr+"/")
	go func() {
		resp, err := http.Get("http://" + addr + "/slow") //nolint:noctx // ends with the test's release
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0 when the drain overruns", code)
	}
	_ = srv.Close()
	if !strings.Contains(buf.String(), "drain exceeded") {
		t.Errorf("log = %q, want the overrun warning", buf.String())
	}
}
