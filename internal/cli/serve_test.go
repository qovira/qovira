package cli

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/qovira/qovira/internal/store"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// freePort allocates an ephemeral TCP port on 127.0.0.1, releases it, and
// returns the "127.0.0.1:<port>" address string. There is an inherent TOCTOU
// window, but it is small and sufficient for single-machine tests.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("freePort close: %v", err)
	}
	return addr
}

// preMigrate opens a SQLCipher store at the path serve would use, runs all
// pending migrations, and closes it. This lets the subsequent serve invocation
// use AUTO_MIGRATE=false, which avoids migration running under a cancelled
// context (which would return "context canceled" from runner.Up).
func preMigrate(t *testing.T, dataDir string) {
	t.Helper()
	s, err := store.Open(store.Config{
		Path:         store.DBPath(dataDir),
		Key:          adminTestKey,
		ReadPoolSize: 1,
	})
	if err != nil {
		t.Fatalf("preMigrate store.Open: %v", err)
	}
	defer func() {
		if cerr := s.Close(); cerr != nil {
			t.Errorf("preMigrate store.Close: %v", cerr)
		}
	}()
	runner := store.NewRunner()
	if err = runner.Up(context.Background(), s.Writer()); err != nil {
		t.Fatalf("preMigrate runner.Up: %v", err)
	}
}

// serveEnv returns the minimal env required for the serve command to open the
// store and bind its HTTP server. Pass autoMigrate=false after preMigrate to
// avoid running migrations under a cancelled context.
func serveEnv(dataDir, httpAddr string, autoMigrate bool) map[string]string {
	am := "false"
	if autoMigrate {
		am = "true"
	}
	return map[string]string{
		"QOVIRA_MASTER_KEY":   adminTestKey, // reuse constant from admin helpers
		"QOVIRA_DATA_DIR":     dataDir,
		"QOVIRA_HTTP_ADDR":    httpAddr,
		"QOVIRA_AUTO_MIGRATE": am,
	}
}

// ── serve wiring ──────────────────────────────────────────────────────────────

// TestServe_CleanStartupAndShutdown verifies that the serve command wires the
// full dependency graph without panicking and shuts down cleanly when its
// context is cancelled immediately after the server becomes ready.
//
// The serve RunE blocks in a.Run until the context is cancelled. We set a
// pre-cancelled context on the root command so that signal.NotifyContext (which
// wraps cmd.Context()) starts done, app.New succeeds (construction path), and
// a.Run returns immediately through the shutdown path. This exercises:
//   - config.Load + store.Open + token validator + auth module ctor + app.New
//   - a.Run → clean shutdown with no error
//
// AUTO_MIGRATE is false because migrations run in app.New before a.Run, and
// running goose with a cancelled context returns "context canceled". We
// pre-migrate the store so the schema is present.
func TestServe_CleanStartupAndShutdown(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel().
	dataDir := t.TempDir()
	addr := freePort(t)

	// Apply migrations under a live context before handing the DB to serve.
	preMigrate(t, dataDir)

	env := serveEnv(dataDir, addr, false)
	for k, v := range env {
		t.Setenv(k, v)
	}

	// Pre-cancel the context so serve RunE's signal.NotifyContext wraps an
	// already-done parent: app.New constructs, then a.Run returns immediately.
	cancelledCtx, cancelNow := context.WithCancel(context.Background())
	cancelNow()

	root := newRootCmd()
	root.SetContext(cancelledCtx)
	root.SetArgs([]string{"serve"})

	var outBuf, errBuf strings.Builder
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)

	// Guard against a hang if the wiring blocks unexpectedly.
	done := make(chan error, 1)
	go func() { done <- root.Execute() }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve with pre-cancelled context returned error: %v\nstderr: %s",
				err, errBuf.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return within 15 s; wiring likely blocked")
	}
}

// TestServe_NoMasterKey verifies that serve fails fast when QOVIRA_MASTER_KEY
// is absent — consistent with TestCommandWiring, placed here for completeness.
func TestServe_NoMasterKey(t *testing.T) {
	t.Parallel()

	_, stderr, err := runCmd(t, "serve")
	if err == nil {
		t.Fatal("expected error when QOVIRA_MASTER_KEY is absent, got nil")
	}
	if !strings.Contains(err.Error(), "master_key") && !strings.Contains(stderr, "master_key") {
		t.Errorf("expected master_key error; err=%v stderr=%q", err, stderr)
	}
}
