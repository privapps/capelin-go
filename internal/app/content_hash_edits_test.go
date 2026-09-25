package app

import (
	"capelin-go/internal/contracts"
	"capelin-go/internal/tools"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFileToolContractRequiresContentHash(t *testing.T) {
	var edit contracts.Tool
	for _, candidate := range tools.Build(map[string]bool{toolEditFile: true}) {
		if candidate.Function.Name == toolEditFile {
			edit = candidate
			break
		}
	}
	if edit.Function.Name != toolEditFile {
		t.Fatal("edit_file was missing from the enabled tool catalog")
	}

	properties, ok := edit.Function.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("edit_file properties = %#v, want an object", edit.Function.Parameters["properties"])
	}
	if _, ok := properties["content_hash"]; !ok {
		t.Fatal("edit_file contract is missing content_hash")
	}
	required, ok := edit.Function.Parameters["required"].([]string)
	if !ok {
		t.Fatalf("edit_file required = %#v, want []string", edit.Function.Parameters["required"])
	}
	for _, field := range []string{"path", "old_str", "new_str", "content_hash"} {
		found := false
		for _, item := range required {
			if item == field {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("edit_file required fields = %v, missing %q", required, field)
		}
	}
}

func TestFileToolReadReportsFullContentHashAndKeepsLineRanges(t *testing.T) {
	tests := []struct {
		name      string
		content   []byte
		startLine int
		endLine   int
		lines     string
	}{
		{name: "normal text", content: []byte("alpha\nbeta"), lines: "1. alpha\n2. beta"},
		{name: "empty", content: nil, lines: "1. "},
		{name: "trailing newline", content: []byte("alpha\n"), lines: "1. alpha\n2. "},
		{name: "unicode", content: []byte("héllo 世界"), lines: "1. héllo 世界"},
		{name: "crlf", content: []byte("first\r\nsecond\r\n"), lines: "1. first\r\n2. second\r\n3. "},
		{name: "line range", content: []byte("one\ntwo\nthree\n"), startLine: 2, endLine: 2, lines: "2. two"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, path := newFileToolTestApp(t, test.content, true)
			arguments := map[string]any{"path": path}
			if test.startLine != 0 {
				arguments["start_line"] = test.startLine
			}
			if test.endLine != 0 {
				arguments["end_line"] = test.endLine
			}

			out, err := runFileToolCall(t, a, toolReadFile, arguments)
			if err != nil {
				t.Fatalf("read_file: %v", err)
			}
			wantHash := rawContentHash(test.content)
			want := fmt.Sprintf("content_hash: %s\n\n%s", wantHash, test.lines)
			if out != want {
				t.Fatalf("read_file output = %q, want %q", out, want)
			}
		})
	}
}

func TestFileToolEditUsesReadHashAndReturnsPostEditHash(t *testing.T) {
	initial := []byte("before\nkeep\n")
	a, path := newFileToolTestApp(t, initial, true)

	readOutput, err := runFileToolCall(t, a, toolReadFile, map[string]any{"path": path})
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	initialHash := hashFromReadOutput(t, readOutput)

	editOutput, err := runFileToolCall(t, a, toolEditFile, map[string]any{
		"path": path, "old_str": "before", "new_str": "after", "content_hash": initialHash,
	})
	if err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	updated := []byte("after\nkeep\n")
	if got, err := os.ReadFile(filepath.Join(a.cfg.workspaceRoot, path)); err != nil || string(got) != string(updated) {
		t.Fatalf("edited bytes = %q, err=%v; want %q", got, err, updated)
	}
	postHash := rawContentHash(updated)
	if !strings.Contains(editOutput, "content_hash: "+postHash) {
		t.Fatalf("edit_file output = %q, missing post-edit hash %q", editOutput, postHash)
	}

	if _, err := runFileToolCall(t, a, toolEditFile, map[string]any{
		"path": path, "old_str": "keep", "new_str": "changed", "content_hash": initialHash,
	}); err == nil || !strings.Contains(err.Error(), "snapshot is stale") || !strings.Contains(err.Error(), "reread") {
		t.Fatalf("stale edit error = %v, want actionable stale snapshot error", err)
	}
	if got, err := os.ReadFile(filepath.Join(a.cfg.workspaceRoot, path)); err != nil || string(got) != string(updated) {
		t.Fatalf("stale edit changed bytes = %q, err=%v; want %q", got, err, updated)
	}

	// The returned post-edit hash is a usable guard for the next edit.
	if _, err := runFileToolCall(t, a, toolEditFile, map[string]any{
		"path": path, "old_str": "keep", "new_str": "changed", "content_hash": postHash,
	}); err != nil {
		t.Fatalf("edit_file with returned post-edit hash: %v", err)
	}
}

func TestFileToolEditRejectsInvalidHashesWithoutMutation(t *testing.T) {
	initial := []byte("unique value")
	tests := []struct {
		name        string
		contentHash any
		want        string
	}{
		{name: "missing", contentHash: nil, want: "content_hash is required"},
		{name: "raw digest", contentHash: strings.Repeat("a", sha256.Size*2), want: "content_hash is invalid"},
		{name: "short sha256", contentHash: "sha256:abcd", want: "content_hash is invalid"},
		{name: "uppercase digest", contentHash: "sha256:" + strings.Repeat("A", sha256.Size*2), want: "content_hash is invalid"},
		{name: "short hash padding", contentHash: rawContentHash(initial) + "=", want: "content_hash is invalid"},
		{name: "short hash standard base64", contentHash: "sha256-128:" + strings.Repeat("+", 22), want: "content_hash is invalid"},
		{name: "unsupported algorithm", contentHash: "md5:" + strings.Repeat("a", 32), want: "unsupported"},
		{name: "md4 algorithm", contentHash: "md4:" + strings.Repeat("a", 32), want: "unsupported"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, path := newFileToolTestApp(t, initial, true)
			arguments := map[string]any{
				"path": path, "old_str": "unique value", "new_str": "changed",
			}
			if test.contentHash != nil {
				arguments["content_hash"] = test.contentHash
			}
			if _, err := runFileToolCall(t, a, toolEditFile, arguments); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("edit_file error = %v, want substring %q", err, test.want)
			}
			if got, err := os.ReadFile(filepath.Join(a.cfg.workspaceRoot, path)); err != nil || string(got) != string(initial) {
				t.Fatalf("rejected edit bytes = %q, err=%v; want %q", got, err, initial)
			}
		})
	}
}

func TestFileToolEditAcceptsLegacyFullHashAndReturnsShortHash(t *testing.T) {
	initial := []byte("before\nkeep\n")
	a, path := newFileToolTestApp(t, initial, true)

	legacyHash := legacyContentHash(initial)
	editOutput, err := runFileToolCall(t, a, toolEditFile, map[string]any{
		"path": path, "old_str": "before", "new_str": "after", "content_hash": legacyHash,
	})
	if err != nil {
		t.Fatalf("edit_file with legacy hash: %v", err)
	}
	updated := []byte("after\nkeep\n")
	shortHash := rawContentHash(updated)
	if !strings.Contains(editOutput, "content_hash: "+shortHash) {
		t.Fatalf("edit_file output = %q, missing canonical short hash %q", editOutput, shortHash)
	}

	// A syntactically valid legacy digest must still match all 256 bits, not
	// merely the 128 bits represented by the compact format.
	wrongLegacy := legacyContentHash(updated)
	last := wrongLegacy[len(wrongLegacy)-1]
	replacement := byte('0')
	if last == replacement {
		replacement = '1'
	}
	wrongLegacy = wrongLegacy[:len(wrongLegacy)-1] + string(replacement)
	if _, err := runFileToolCall(t, a, toolEditFile, map[string]any{
		"path": path, "old_str": "keep", "new_str": "changed", "content_hash": wrongLegacy,
	}); err == nil || !strings.Contains(err.Error(), "snapshot is stale") {
		t.Fatalf("edit_file wrong legacy hash error = %v, want stale snapshot", err)
	}
	if got, err := os.ReadFile(filepath.Join(a.cfg.workspaceRoot, path)); err != nil || string(got) != string(updated) {
		t.Fatalf("wrong legacy hash changed bytes = %q, err=%v; want %q", got, err, updated)
	}
}

func TestFileToolEditPreservesExactOnceFailureAndPermissionChecks(t *testing.T) {
	initial := []byte("same same")
	a, path := newFileToolTestApp(t, initial, true)
	hash := rawContentHash(initial)
	for _, old := range []string{"missing", "same"} {
		if _, err := runFileToolCall(t, a, toolEditFile, map[string]any{
			"path": path, "old_str": old, "new_str": "replacement", "content_hash": hash,
		}); err == nil || !strings.Contains(err.Error(), "old_str") {
			t.Fatalf("edit_file old_str=%q error = %v, want exact-match error", old, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(a.cfg.workspaceRoot, path)); err != nil || string(got) != string(initial) {
		t.Fatalf("exact-match failure changed bytes = %q, err=%v; want %q", got, err, initial)
	}

	denied, _ := newFileToolTestApp(t, []byte("protected"), false)
	if _, err := runFileToolCall(t, denied, toolEditFile, map[string]any{
		"path": path, "old_str": "protected", "new_str": "changed", "content_hash": rawContentHash([]byte("protected")),
	}); err == nil || !strings.Contains(err.Error(), "--allow-tool edit_file") {
		t.Fatalf("disabled edit error = %v, want permission guidance", err)
	}
}

func TestFileToolConcurrentIndependentEditsRejectOneStaleSnapshot(t *testing.T) {
	root := t.TempDir()
	path := "notes.txt"
	filePath := filepath.Join(root, path)
	initial := []byte("before\n")
	if err := os.WriteFile(filePath, initial, 0o600); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, replacement := range []string{"first", "second"} {
		group.Add(1)
		go func(replacement string) {
			defer group.Done()
			<-start
			_, err := runEditFile(root, false, editFileArgs{
				Path: path, OldStr: "before", NewStr: replacement, ContentHash: rawContentHash(initial),
			})
			results <- err
		}(replacement)
	}
	close(start)
	group.Wait()
	close(results)

	var successes, stale int
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if strings.Contains(err.Error(), "snapshot is stale") {
			stale++
			continue
		}
		t.Fatalf("concurrent edit error = %v, want stale snapshot", err)
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("concurrent edit outcomes = successes %d, stale %d; want one of each", successes, stale)
	}
	got, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first\n" && string(got) != "second\n" {
		t.Fatalf("concurrent edit bytes = %q, want one successful replacement", got)
	}
}

func TestFileToolErrorsAreRecoverableThroughApplicationRunner(t *testing.T) {
	a, path := newFileToolTestApp(t, []byte("before"), true)
	initial := rawContentHash([]byte("before"))
	if err := os.WriteFile(filepath.Join(a.cfg.workspaceRoot, path), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := a.rootRuntime()
	result := newAppToolCapability(nil, a, runtime).Run(context.Background(), []contracts.ToolCall{{
		ID: "stale-edit",
		Function: contracts.FunctionCall{
			Name: toolEditFile,
			Arguments: mustFileJSON(map[string]any{
				"path": path, "old_str": "changed", "new_str": "new", "content_hash": initial,
			}),
		},
	}})[0]
	if !result.IsError || result.Recovery == nil || !strings.Contains(result.Output, "snapshot is stale") {
		t.Fatalf("stale application result = %#v, want recoverable stale error", result)
	}
}

func newFileToolTestApp(t *testing.T, content []byte, allowEdit bool) (*app, string) {
	t.Helper()
	root := t.TempDir()
	path := "notes.txt"
	if err := os.WriteFile(filepath.Join(root, path), content, 0o600); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{toolReadFile: true}
	if allowEdit {
		allowed[toolEditFile] = true
	}
	return &app{cfg: config{
		workspaceRoot:   root,
		allowedTools:    allowed,
		toolMaxParallel: 1,
		toolTimeoutSec:  5,
	}}, path
}

func runFileToolCall(t *testing.T, a *app, name string, arguments map[string]any) (string, error) {
	t.Helper()
	return a.runTool(context.Background(), contracts.ToolCall{
		Type:     "function",
		Function: contracts.FunctionCall{Name: name, Arguments: mustFileJSON(arguments)},
	})
}

func mustFileJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func rawContentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256-128:" + base64.RawURLEncoding.EncodeToString(sum[:sha256.Size/2])
}

func legacyContentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum[:])
}

func hashFromReadOutput(t *testing.T, output string) string {
	t.Helper()
	line, _, ok := strings.Cut(output, "\n")
	if !ok {
		t.Fatalf("read output has no content hash header: %q", output)
	}
	const prefix = "content_hash: "
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("read output starts with %q, want %q", line, prefix)
	}
	return strings.TrimPrefix(line, prefix)
}
