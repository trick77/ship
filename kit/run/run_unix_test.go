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

// parentStopChild is the child process of the parent-cancel test: its parent context ends first, then it drains
// without calling stop and exits on its own.
const parentStopChild = "RUN_TEST_PARENT_STOP_CHILD"

func TestContext_whenParentCancelledThenSIGTERM_thenNotKilled(t *testing.T) {
	if os.Getenv(parentStopChild) == "1" {
		parent, cancel := context.WithCancel(context.Background())
		ctx, stop := Context(parent)
		defer stop()
		cancel()
		<-ctx.Done()
		// Lets a release AfterFunc run before the signal; only the test's power to catch a release hangs on it, the
		// kept handler passes regardless.
		time.Sleep(200 * time.Millisecond)
		fmt.Println("cancelled")
		in := bufio.NewScanner(os.Stdin)
		if !in.Scan() || in.Text() != "signalled" {
			os.Exit(2)
		}
		// The SIGTERM went out before "signalled": lets its delivery kill a released handler before the exit.
		time.Sleep(500 * time.Millisecond)
		fmt.Println("survived")
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestContext_whenParentCancelledThenSIGTERM_thenNotKilled$")
	cmd.Env = append(os.Environ(), parentStopChild+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := cmd.StdinPipe()
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
			t.Fatalf("child said %q, want %q (child %v)", lines.Text(), want, cmd.ProcessState)
		}
	}
	expect("cancelled")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(in, "signalled"); err != nil {
		t.Fatal(err)
	}
	expect("survived")
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("child ended %v, want exit 0: the SIGTERM after the parent's cancel killed it", cmd.ProcessState)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
		t.Fatal("child did not exit on its own")
	}
}
