//go:build unix

package httpx

import (
	"context"
	"net/http"
	"os"
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
