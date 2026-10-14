//go:build unix

package run

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Unix only: Windows cannot send itself SIGTERM.
func TestContext_whenSIGTERM_thenDone(t *testing.T) {
	ctx, stop := Context(context.Background())
	defer stop()
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not end the context")
	}
}

// slowStopChild is the child process of the second-signal test: it hangs on after the first stop, as a slow drain
// or cleanup would, without calling stop.
const slowStopChild = "RUN_TEST_SLOW_STOP_CHILD"

func TestContext_whenSecondSIGTERMDuringSlowStop_thenKilled(t *testing.T) {
	if os.Getenv(slowStopChild) == "1" {
		ctx, stop := Context(context.Background())
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("stopping")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestContext_whenSecondSIGTERMDuringSlowStop_thenKilled$")
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
			_ = cmd.Wait()
			t.Fatalf("child said %q, want %q", lines.Text(), want)
		}
	}
	expect("ready")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	expect("stopping")
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	// The handler goes asynchronously after the first signal: repeat the second until it lands.
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
