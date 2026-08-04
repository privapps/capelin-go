package policy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContainsDangerousPattern(t *testing.T) {
	tests := []struct {
		name    string
		command string
		args    []string
		blocked bool
	}{
		{name: "shell command", command: "bash", args: []string{"-lc", "echo hi"}, blocked: true},
		{name: "command metacharacters", command: "go;rm", args: []string{"test"}, blocked: true},
		{name: "literal argument punctuation", command: "go", args: []string{"test", "./...;rm"}},
		{name: "argument nul", command: "go", args: []string{"test", "a\x00b"}, blocked: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContainsDangerousPattern(tt.command, tt.args); got != tt.blocked {
				t.Fatalf("ContainsDangerousPattern(%q, %q) = %v, want %v", tt.command, tt.args, got, tt.blocked)
			}
		})
	}
}

func TestResolveWorkspacePathConfinement(t *testing.T) {
	root := t.TempDir()
	if _, err := ResolveWorkspacePath(root, "../outside"); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
	if _, err := ResolveWorkspacePath(root, filepath.Join(root, "outside")); err == nil {
		t.Fatal("expected absolute path to be rejected")
	}

	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks not supported on this platform: %v", err)
	}
	if _, err := ResolveWorkspacePath(root, "link/secret.txt"); err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
}

func TestResolveWorkspacePathAllowsWorkspacePath(t *testing.T) {
	root := t.TempDir()
	resolved, err := ResolveWorkspacePath(root, "nested/file.txt")
	if err != nil {
		t.Fatalf("ResolveWorkspacePath: %v", err)
	}
	want := filepath.Join(root, "nested", "file.txt")
	if resolved != want {
		t.Fatalf("resolved path = %q, want %q", resolved, want)
	}
}
