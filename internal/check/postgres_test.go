package check

import (
	"strings"
	"testing"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
)

// fakeDocker answers run with a container id, port with a loopback port, exec (pg_isready) with $EXEC_RC.
const fakeDocker = `case "$1" in
run) [ -n "$RUN_FAIL" ] && exit 1; echo cid1 ;;
port) echo "${PORT_OUT-127.0.0.1:55432
[::1]:55432}" ;;
exec) exit "${EXEC_RC:-0}" ;;
esac`

func pgRunner(v string) runner { return runner{root: ".", c: &config.Config{Postgres: v}} }

func TestPostgres_whenUnsetOrDSNGiven_thenNoContainer(t *testing.T) {
	log := fakeBin(t, "docker", fakeDocker)
	env, stop, err := pgRunner("").postgres()
	if err != nil || env != nil {
		t.Fatalf("%v %v", env, err)
	}
	stop()
	t.Setenv(TestDSN, "postgres://elsewhere")
	env, stop, err = pgRunner("17").postgres()
	if err != nil || env != nil {
		t.Fatalf("%v %v", env, err)
	}
	stop()
	if calls := readLog(t, log); calls != "" {
		t.Fatalf("docker ran: %s", calls)
	}
}

func TestPostgres_whenStarted_thenDSNAndRemovedOnce(t *testing.T) {
	t.Setenv(TestDSN, "")
	log := fakeBin(t, "docker", fakeDocker)
	env, stop, err := pgRunner("17").postgres()
	if err != nil {
		t.Fatal(err)
	}
	if len(env) != 1 || env[0] != TestDSN+"=postgres://postgres:ship@127.0.0.1:55432/postgres?sslmode=disable" {
		t.Fatalf("env: %v", env)
	}
	stop()
	stop()
	calls := readLog(t, log)
	if !strings.Contains(calls, "run -d --rm -p 127.0.0.1::5432 -e POSTGRES_PASSWORD=ship postgres:17-alpine\n") ||
		!strings.Contains(calls, "exec cid1 pg_isready -q -h 127.0.0.1 -U postgres\n") ||
		strings.Count(calls, "rm -f cid1\n") != 1 {
		t.Fatalf("calls:\n%s", calls)
	}
}

func TestPostgres_whenCompose_thenServiceImage(t *testing.T) {
	t.Setenv(TestDSN, "")
	log := fakeBin(t, "docker", fakeDocker)
	dir := t.TempDir()
	if _, err := gitx.Git(dir, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	r := runner{root: dir, c: &config.Config{Postgres: "compose:db"}}
	if _, _, err := r.postgres(); err == nil || !strings.Contains(err.Error(), "compose:db: no compose file") {
		t.Fatalf("no compose file: %v", err)
	}
	if calls := readLog(t, log); calls != "" {
		t.Fatalf("docker ran: %s", calls)
	}
	writeFile(t, dir, "compose.yaml", "services:\n  db:\n    image: timescale/timescaledb:2.29.1-pg17\n")
	_, stop, err := r.postgres()
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if calls := readLog(t, log); !strings.Contains(calls, "POSTGRES_PASSWORD=ship timescale/timescaledb:2.29.1-pg17\n") {
		t.Fatalf("calls:\n%s", calls)
	}
}

func TestPostgres_whenFailing_thenErrorAndRemoved(t *testing.T) {
	t.Setenv(TestDSN, "")
	old := pgWait
	pgWait = 0
	t.Cleanup(func() { pgWait = old })
	log := fakeBin(t, "docker", fakeDocker)
	t.Setenv("EXEC_RC", "2")
	if _, _, err := pgRunner("16").postgres(); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("never ready: %v", err)
	}
	if !strings.Contains(readLog(t, log), "rm -f cid1") {
		t.Fatal("unready container left running")
	}
	t.Setenv("EXEC_RC", "0")
	t.Setenv("PORT_OUT", "garbage")
	if _, _, err := pgRunner("16").postgres(); err == nil || !strings.Contains(err.Error(), "docker port") {
		t.Fatalf("bad port output: %v", err)
	}
	t.Setenv("RUN_FAIL", "1")
	if _, _, err := pgRunner("16").postgres(); err == nil || !strings.Contains(err.Error(), "docker run") {
		t.Fatalf("docker run failure: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, _, err := pgRunner("16").postgres(); err == nil || !strings.Contains(err.Error(), "needs docker") {
		t.Fatalf("no docker: %v", err)
	}
}
