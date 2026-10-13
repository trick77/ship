//go:build unix

package httpx

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Unix only: Windows cannot send itself SIGTERM.
func TestMain_whenSIGTERMDuringBuild_thenBuildSeesItZeroAndCleanup(t *testing.T) {
	entered, cleaned := make(chan struct{}), false
	build := func(ctx context.Context) (*http.Server, func(), error) {
		close(entered)
		select {
		case <-ctx.Done():
			return nil, func() { cleaned = true }, ctx.Err()
		case <-time.After(5 * time.Second):
			t.Error("Build's ctx did not end on SIGTERM")
			return nil, nil, nil
		}
	}
	done := make(chan int, 1)
	go func() {
		done <- Main(context.Background(), mainFlags(), nil, MainOptions{Addr: "127.0.0.1:1", Build: build, Log: quietLog()})
	}()
	<-entered
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Errorf("Main = %d, want 0 for a startup cut short by a stop", code)
	}
	if !cleaned {
		t.Error("cleanup did not run")
	}
}

// slowStopChild is the child process of the second-signal test: its Build hangs on after the stop, as a slow
// startup or cleanup would.
const slowStopChild = "HTTPX_TEST_SLOW_STOP_CHILD"

func TestMain_whenSecondSIGTERMDuringSlowStop_thenKilled(t *testing.T) {
	if os.Getenv(slowStopChild) == "1" {
		build := func(ctx context.Context) (*http.Server, func(), error) {
			fmt.Println("building")
			<-ctx.Done()
			fmt.Println("stopping")
			time.Sleep(30 * time.Second)
			return nil, nil, ctx.Err()
		}
		os.Exit(Main(context.Background(), mainFlags(), nil, MainOptions{Addr: "127.0.0.1:1", Build: build, Log: quietLog()}))
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestMain_whenSecondSIGTERMDuringSlowStop_thenKilled$")
	cmd.Env = append(os.Environ(), slowStopChild+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(out)
	expect := func(want string) {
		t.Helper()
		if !lines.Scan() || lines.Text() != want {
			_ = cmd.Process.Kill()
			t.Fatalf("child said %q, want %q", lines.Text(), want)
		}
	}
	expect("building")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	expect("stopping")
	expectKilledBySecondSIGTERM(t, cmd)
}

// expectKilledBySecondSIGTERM repeats SIGTERM to the child, which has had its first, until it dies of one, and fails
// the test when that takes 10s: the signal handler stayed in place.
func expectKilledBySecondSIGTERM(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// The handler goes asynchronously after the first stop: repeat the second signal until it lands.
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-exited:
			ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
				t.Errorf("child ended %v, want killed by SIGTERM", cmd.ProcessState)
			}
			return
		case <-tick.C:
			_ = cmd.Process.Signal(syscall.SIGTERM)
		case <-deadline:
			_ = cmd.Process.Kill()
			<-exited
			t.Fatal("a second SIGTERM during the stop did not kill the process: the signal handler was still in place")
		}
	}
}

// drainChild is the child process of the second-signal-during-drain test: "serve" or "main", which of the two runs
// the server whose handler blocks through the drain.
const drainChild = "HTTPX_TEST_DRAIN_CHILD"

func TestServeAndMain_whenSecondSIGTERMDuringDrain_thenKilled(t *testing.T) {
	if mode := os.Getenv(drainChild); mode != "" {
		addr := freeAddr(t)
		srv := NewServer(addr, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/hold" {
				return
			}
			fmt.Println("handling")
			time.Sleep(30 * time.Second) // deaf to its request's context: the drain waits it out
		}))
		go func() {
			waitUp(t, "http://"+addr+"/")
			getHold(addr)
		}()
		if mode == "serve" {
			_ = Serve(context.Background(), srv, 30*time.Second)
			os.Exit(0)
		}
		build := func(context.Context) (*http.Server, func(), error) { return srv, nil, nil }
		os.Exit(Main(context.Background(), mainFlags(), nil, MainOptions{Addr: addr, Build: build, Drain: 30 * time.Second, Log: quietLog()}))
	}
	for _, mode := range []string{"serve", "main"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestServeAndMain_whenSecondSIGTERMDuringDrain_thenKilled$")
			cmd.Env = append(os.Environ(), drainChild+"="+mode)
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			lines := bufio.NewScanner(out)
			if !lines.Scan() || lines.Text() != "handling" {
				_ = cmd.Process.Kill()
				t.Fatalf("child said %q, want %q", lines.Text(), "handling")
			}
			// The first starts the drain, which the deaf handler holds 30s; a later one must kill.
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			expectKilledBySecondSIGTERM(t, cmd)
		})
	}
}
