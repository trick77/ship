//go:build unix

package run

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// Unix only: Windows cannot send itself SIGTERM.
func TestContext_whenSIGTERM_thenDoneWithTheSignalAsCause(t *testing.T) {
	ctx, stop := Context(context.Background())
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not end the context")
	}
	if err := sigtermCause(ctx); err != nil {
		t.Fatal(err)
	}
}

// sigtermCause says what is wrong with ctx's cause unless SIGTERM ended it.
func sigtermCause(ctx context.Context) error {
	cause := context.Cause(ctx)
	var se signalError
	if !errors.As(cause, &se) || se.sig != syscall.SIGTERM || !errors.Is(cause, context.Canceled) {
		return fmt.Errorf("cause %w, want the SIGTERM", cause)
	}
	return nil
}

// childEnv names the test a re-executed test binary runs as the child process: signals that kill must not hit the
// test process itself.
const childEnv = "RUN_TEST_CHILD"

// isChild reports whether this process is t's child. A child says what it saw line by line on stdout and, unless a
// signal kills it, exits on its own.
func isChild(t *testing.T) bool { return os.Getenv(childEnv) == t.Name() }

// hang keeps a child alive as a slow drain or cleanup would, without calling stop; only a kill ends it in time.
func hang() {
	time.Sleep(time.Minute)
	os.Exit(3)
}

type child struct {
	t     *testing.T
	cmd   *exec.Cmd
	lines *bufio.Scanner
	exit  chan struct{}
}

func startChild(t *testing.T) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), childEnv+"="+t.Name())
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{t: t, cmd: cmd, lines: bufio.NewScanner(out)}
	t.Cleanup(func() {
		if c.exit == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return c
}

func (c *child) expect(want string) {
	c.t.Helper()
	if !c.lines.Scan() || c.lines.Text() != want {
		c.t.Fatalf("child said %q, want %q", c.lines.Text(), want)
	}
}

func (c *child) signal() {
	c.t.Helper()
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		c.t.Fatal(err)
	}
}

func (c *child) wait() {
	c.exit = make(chan struct{})
	go func() {
		_ = c.cmd.Wait()
		close(c.exit)
	}()
}

// killedBySIGTERM sends SIGTERM until one kills the child: a Context lets go of the signal asynchronously after the
// first, so a later one may still land before that.
func (c *child) killedBySIGTERM() {
	c.t.Helper()
	c.wait()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-c.exit:
			ws, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
				c.t.Errorf("child ended %v, want killed by SIGTERM", c.cmd.ProcessState)
			}
			return
		case <-tick.C:
			_ = c.cmd.Process.Signal(syscall.SIGTERM)
		case <-deadline:
			_ = c.cmd.Process.Kill()
			<-c.exit
			c.t.Fatal("no later SIGTERM killed the child: a signal handler was still in place")
		}
	}
}

func TestContext_whenSecondSIGTERM_thenKilled(t *testing.T) {
	if isChild(t) {
		ctx, stop := Context(context.Background())
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		if err := sigtermCause(ctx); err != nil {
			fmt.Println(err)
			os.Exit(2)
		}
		fmt.Println("stopping")
		hang()
	}
	c := startChild(t)
	c.expect("ready")
	c.signal()
	c.expect("stopping")
	c.killedBySIGTERM()
}

func TestContext_whenParentCancelledThenSIGTERM_thenSurvivesAndSecondKills(t *testing.T) {
	if isChild(t) {
		parent, cancel := context.WithCancel(context.Background())
		ctx, stop := Context(parent)
		defer stop()
		// Only tells the child the first SIGTERM arrived. While it listens no SIGTERM kills, so it lets go at once;
		// what the second SIGTERM's kill pins is that Context let go too.
		seen := make(chan os.Signal, 1)
		signal.Notify(seen, syscall.SIGTERM)
		cancel()
		<-ctx.Done()
		var se signalError
		if cause := context.Cause(ctx); errors.As(cause, &se) || !errors.Is(cause, context.Canceled) {
			fmt.Println("cause:", cause)
			os.Exit(2)
		}
		fmt.Println("cancelled")
		<-seen
		signal.Stop(seen)
		fmt.Println("survived")
		hang()
	}
	c := startChild(t)
	c.expect("cancelled")
	c.signal()
	c.expect("survived")
	c.killedBySIGTERM()
}

func TestContext_whenNestedThenSIGTERM_thenBothDoneAndSecondKills(t *testing.T) {
	if isChild(t) {
		outer, stopOuter := Context(context.Background())
		defer stopOuter()
		inner, stopInner := Context(outer)
		defer stopInner()
		fmt.Println("ready")
		<-outer.Done()
		<-inner.Done()
		for _, ctx := range []context.Context{outer, inner} {
			if err := sigtermCause(ctx); err != nil {
				fmt.Println(err)
				os.Exit(2)
			}
		}
		fmt.Println("stopping")
		hang()
	}
	c := startChild(t)
	c.expect("ready")
	c.signal()
	c.expect("stopping")
	c.killedBySIGTERM()
}

func TestContext_whenStoppedThenSIGTERM_thenKilled(t *testing.T) {
	if isChild(t) {
		ctx, stop := Context(context.Background())
		stop()
		stop()
		if err := ctx.Err(); err == nil {
			fmt.Println("stop did not end the context")
			os.Exit(2)
		}
		fmt.Println("stopped")
		hang()
	}
	c := startChild(t)
	c.expect("stopped")
	c.killedBySIGTERM()
}
