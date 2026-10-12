package check

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// TestDSN names the env var go test finds the Postgres DSN in. Set before check runs (CI's container, a server of
// one's own), it is used as is and no container starts.
const TestDSN = "SHIP_TEST_DSN"

// pgWait bounds the wait for a fresh container to accept connections (an image pull happens before, in docker run).
var pgWait = 60 * time.Second

// postgres starts postgres:<ship.yaml postgres>-alpine for go test unless SHIP_TEST_DSN is set, and returns the
// env go test gets and a stop func (never nil, safe to call twice). The container is removed by stop, on SIGINT or
// SIGTERM, and by docker itself (--rm) should ship die harder.
func (r runner) postgres() (env []string, stop func(), err error) {
	stop = func() {}
	v := r.c.Postgres
	switch {
	case v == "":
		fmt.Println("(none)")
		return nil, stop, nil
	case os.Getenv(TestDSN) != "":
		fmt.Printf("ship: %s set, no container\n", TestDSN)
		return nil, stop, nil
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, stop, fmt.Errorf("postgres %s needs docker on PATH (or %s pointing at a server)", v, TestDSN)
	}
	// Loopback only, a free port: never reachable from the network, never clashing with a local server.
	id, err := docker("run", "-d", "--rm", "-p", "127.0.0.1::5432", "-e", "POSTGRES_PASSWORD=ship",
		"postgres:"+v+"-alpine")
	if err != nil {
		return nil, stop, err
	}
	var once sync.Once
	sig := make(chan os.Signal, 1)
	stop = func() {
		once.Do(func() {
			signal.Stop(sig)
			close(sig)
			_, _ = docker("rm", "-f", id)
		})
	}
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		if _, ok := <-sig; ok {
			stop()
			fmt.Fprintln(os.Stderr, "ship: interrupted, postgres container removed")
			os.Exit(130)
		}
	}()
	dsn, err := pgReady(id)
	if err != nil {
		stop()
		return nil, func() {}, err
	}
	fmt.Println("ship: postgres", v, "at", dsn)
	return []string{TestDSN + "=" + dsn}, stop, nil
}

// pgReady waits for the container to take TCP connections and returns its DSN. pg_isready over TCP, not the
// socket: the image's init runs a socket-only server first, which reads as ready and then restarts.
func pgReady(id string) (string, error) {
	out, err := docker("port", id, "5432/tcp")
	if err != nil {
		return "", err
	}
	first, _, _ := strings.Cut(out, "\n")
	_, port, err := net.SplitHostPort(strings.TrimSpace(first))
	if err != nil {
		return "", fmt.Errorf("docker port %s: %q: %w", id, out, err)
	}
	deadline := time.Now().Add(pgWait)
	for {
		if _, err := docker("exec", id, "pg_isready", "-q", "-h", "127.0.0.1", "-U", "postgres"); err == nil {
			return "postgres://postgres:ship@127.0.0.1:" + port + "/postgres?sslmode=disable", nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("postgres container %s not ready after %s", id, pgWait)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// docker runs docker, stderr streamed (an image pull's progress), and returns trimmed stdout.
func docker(args ...string) (string, error) {
	cmd := exec.Command("docker", args...) //nolint:gosec // docker with ship's own arguments
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w", args[0], err)
	}
	return strings.TrimSpace(out.String()), nil
}
