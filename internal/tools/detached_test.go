package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetachedHelperProcess(t *testing.T) {
	if os.Getenv("CAPELIN_DETACHED_HELPER") != "1" {
		return
	}
	started := os.Getenv("CAPELIN_DETACHED_STARTED")
	finished := os.Getenv("CAPELIN_DETACHED_FINISHED")
	argsFile := os.Getenv("CAPELIN_DETACHED_ARGS")
	cwdFile := os.Getenv("CAPELIN_DETACHED_CWD")
	_ = os.WriteFile(argsFile, []byte(strings.Join(os.Args, "\x00")), 0o600)
	if cwd, err := os.Getwd(); err == nil {
		_ = os.WriteFile(cwdFile, []byte(cwd), 0o600)
	}
	_ = os.WriteFile(started, []byte("started"), 0o600)
	_, _ = os.Stdout.Write([]byte("detached child output must not be inherited\n"))
	time.Sleep(1500 * time.Millisecond)
	_ = os.WriteFile(finished, []byte("finished"), 0o600)
	os.Exit(0)
}

func TestRunDetachedProgramReturnsAfterLaunchAndReapsChild(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	finished := filepath.Join(dir, "finished")
	argsFile := filepath.Join(dir, "args")
	cwdFile := filepath.Join(dir, "cwd")
	t.Setenv("CAPELIN_DETACHED_HELPER", "1")
	t.Setenv("CAPELIN_DETACHED_STARTED", started)
	t.Setenv("CAPELIN_DETACHED_FINISHED", finished)
	t.Setenv("CAPELIN_DETACHED_ARGS", argsFile)
	t.Setenv("CAPELIN_DETACHED_CWD", cwdFile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begin := time.Now()
	err := RunDetachedProgram(ctx, dir, false, ExecuteProgramArgs{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestDetachedHelperProcess", "--", "literal $(not shell)"},
	})
	if err != nil {
		t.Fatalf("RunDetachedProgram: %v", err)
	}
	cancel()
	if elapsed := time.Since(begin); elapsed >= time.Second {
		t.Fatalf("detached launch waited for child completion: %v", elapsed)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("detached child did not start: %v", err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil || !strings.Contains(string(args), "literal $(not shell)") {
		t.Fatalf("detached child arguments = %q, err=%v; want literal argument", args, err)
	}
	cwd, err := os.ReadFile(cwdFile)
	if err != nil || string(cwd) != dir {
		t.Fatalf("detached child cwd = %q, err=%v; want %q", cwd, err, dir)
	}
	if _, err := os.Stat(finished); err == nil {
		t.Fatal("detached launch reported success only after child completion")
	}

	// Keep the child lifecycle contained within this test so a subsequent test
	// cannot observe its completion marker as a false positive.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(finished); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("detached child was not eventually reaped and completed")
}

func TestRunDetachedProgramReportsLaunchFailure(t *testing.T) {
	err := RunDetachedProgram(context.Background(), t.TempDir(), false, ExecuteProgramArgs{
		Command: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	if err == nil || !strings.Contains(err.Error(), "start") {
		t.Fatalf("launch failure = %v, want a start diagnostic", err)
	}
}

func TestRunDetachedProgramPreservesDangerousPatternPolicy(t *testing.T) {
	err := RunDetachedProgram(context.Background(), t.TempDir(), false, ExecuteProgramArgs{
		Command: "sh",
		Args:    []string{"-c", "echo must be blocked"},
	})
	if err == nil || !strings.Contains(err.Error(), "dangerous-pattern") {
		t.Fatalf("dangerous detached command error = %v, want policy rejection", err)
	}
}

func TestRunIdleHookProgramUsesItsOwnWaitTimeoutCeiling(t *testing.T) {
	_, err := RunIdleHookProgram(context.Background(), t.TempDir(), false, ExecuteProgramArgs{
		Command:        os.Args[0],
		Args:           []string{"-test.run=TestNoSuchDetachedHelper"},
		TimeoutSeconds: 121,
	})
	if err != nil {
		t.Fatalf("idle-hook wait adapter rejected a timeout above ordinary execute_program's cap: %v", err)
	}
}
