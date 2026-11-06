package hook

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"charm.land/log/v2"
	"github.com/charmbracelet/soft-serve/pkg/config"
	"github.com/charmbracelet/soft-serve/pkg/db"
	_ "modernc.org/sqlite" // sqlite driver
)

func TestRunCustomHookPropagatesExitError(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Skip("test binary is not executable on this platform")
	}

	t.Setenv("SOFT_SERVE_TEST_HELPER", "1")
	t.Setenv("SOFT_SERVE_TEST_HELPER_EXIT", "1")
	err = runCustomHook(
		context.Background(),
		executable,
		io.Reader(nil),
		io.Discard,
		io.Discard,
		"-test.run=TestCustomHookHelperProcess",
	)
	if err == nil {
		t.Fatal("runCustomHook returned nil for a failing custom hook")
	}

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("runCustomHook error = %T %v, want exec.ExitError", err, err)
	}
}

func TestRunCustomHookIgnoresMissingOrNonExecutableHook(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-hook")
	if err := runCustomHook(context.Background(), missing, nil, io.Discard, io.Discard); err != nil {
		t.Fatalf("runCustomHook(missing) error = %v", err)
	}

	nonExecutable := filepath.Join(t.TempDir(), "non-executable-hook")
	if err := os.WriteFile(nonExecutable, []byte("exit 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runCustomHook(context.Background(), nonExecutable, nil, io.Discard, io.Discard); err != nil {
		t.Fatalf("runCustomHook(non-executable) error = %v", err)
	}
}

func TestCustomHookHelperProcess(t *testing.T) {
	if os.Getenv("SOFT_SERVE_TEST_HELPER") != "1" {
		return
	}
	if os.Getenv("SOFT_SERVE_TEST_HELPER_EXIT") == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}

// When a hook fails, the hook command still closes the database: cobra
// skips the command's PersistentPostRunE, which closes it otherwise.
func TestHookErrorClosesDB(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("custom hook is a shell script")
	}
	t.Setenv("SOFT_SERVE_DATA_PATH", t.TempDir())
	cfg := config.DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	hooksPath := filepath.Join(cfg.DataPath, "hooks")
	if err := os.MkdirAll(hooksPath, 0o755); err != nil {
		t.Fatal(err)
	}
	//nolint: gosec
	if err := os.WriteFile(filepath.Join(hooksPath, "post-update"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := config.WithContext(context.Background(), cfg)
	ctx = log.WithContext(ctx, log.New(io.Discard))

	Command.SetArgs([]string{"post-update"})
	Command.SetIn(strings.NewReader(""))
	Command.SetOut(io.Discard)
	Command.SetErr(io.Discard)
	if err := Command.ExecuteContext(ctx); err == nil {
		t.Fatal("hook returned nil for a failing custom hook")
	}

	dbx := db.FromContext(postUpdateCmd.Context())
	if dbx == nil {
		t.Fatal("no database in the command context")
	}
	if err := dbx.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("database ping returned %v, want it closed", err)
	}
}
