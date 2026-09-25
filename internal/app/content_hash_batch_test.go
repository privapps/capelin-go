package app

import (
	configpkg "capelin-go/internal/config"
	"capelin-go/internal/contracts"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestContentHashBatchSerializesSameFileAndRejectsStaleEdit(t *testing.T) {
	a, root := newContentHashBatchApp(t, 2)
	path := "notes.txt"
	initial := []byte("before\nkeep\n")
	if err := os.WriteFile(filepath.Join(root, path), initial, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := rawContentHash(initial)
	calls := []contracts.ToolCall{
		contentHashEditCall("first", path, "before", "first", hash),
		contentHashEditCall("second", path, "before", "second", hash),
	}

	results := newAppToolCapability(nil, a, a.rootRuntime()).Run(context.Background(), calls)
	if len(results) != len(calls) {
		t.Fatalf("result count = %d, want %d", len(results), len(calls))
	}
	if results[0].IsError {
		t.Fatalf("first same-file edit failed: %s", results[0].Output)
	}
	if !results[1].IsError || !strings.Contains(results[1].Output, "snapshot is stale") {
		t.Fatalf("second same-file edit = %#v, want stale-hash error", results[1])
	}
	got, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("first\nkeep\n")
	if string(got) != string(want) {
		t.Fatalf("same-file batch bytes = %q, want %q", got, want)
	}
}

func TestContentHashBatchRunsIndependentFilesInParallelWhileSameFileWaits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("named-pipe scheduling test requires Unix FIFOs")
	}
	a, root := newContentHashBatchApp(t, 2)
	firstPath := filepath.Join(root, "first.txt")
	secondPath := filepath.Join(root, "second.txt")
	if err := makeContentHashFIFO(firstPath); err != nil {
		t.Fatal(err)
	}
	if err := makeContentHashFIFO(secondPath); err != nil {
		t.Fatal(err)
	}
	firstFeed := newContentHashFIFOWriter(t, firstPath, []byte("first\n"))
	secondFeed := newContentHashFIFOWriter(t, secondPath, []byte("second\n"))
	calls := []contracts.ToolCall{
		contentHashEditCall("first-edit", "first.txt", "first", "FIRST", rawContentHash([]byte("first\n"))),
		contentHashEditCall("stale-first-edit", "first.txt", "first", "stale", rawContentHash([]byte("first\n"))),
		contentHashEditCall("second-edit", "second.txt", "second", "SECOND", rawContentHash([]byte("second\n"))),
	}

	done := make(chan []contracts.ToolResult, 1)
	go func() {
		done <- newAppToolCapability(nil, a, a.rootRuntime()).Run(context.Background(), calls)
	}()
	if err := firstFeed.waitForRead(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := secondFeed.waitForRead(time.Second); err != nil {
		t.Fatal(err)
	}
	firstFeed.release()
	secondFeed.release()

	var results []contracts.ToolResult
	select {
	case results = <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for application batch")
	}
	if results[0].IsError || !results[1].IsError || results[2].IsError {
		t.Fatalf("parallel batch results = %#v, want first/third success and second stale", results)
	}
	first, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != "FIRST\n" || string(second) != "SECOND\n" {
		t.Fatalf("parallel batch bytes = first %q, second %q", first, second)
	}
}

func newContentHashBatchApp(t *testing.T, maxParallel int) (*app, string) {
	t.Helper()
	root := t.TempDir()
	profile := configpkg.RuntimeProfile{
		MaxIterations:   10,
		ToolMaxParallel: maxParallel,
		ToolTimeoutSec:  5,
	}
	a := &app{cfg: config{
		workspaceRoot:    root,
		allowedTools:     map[string]bool{toolEditFile: true},
		ordinaryProfile:  profile,
		profilesResolved: true,
	}}
	return a, root
}

func contentHashEditCall(id, path, oldStr, newStr, contentHash string) contracts.ToolCall {
	arguments, err := json.Marshal(map[string]string{
		"path": path, "old_str": oldStr, "new_str": newStr, "content_hash": contentHash,
	})
	if err != nil {
		panic(err)
	}
	return contracts.ToolCall{ID: id, Function: contracts.FunctionCall{
		Name: toolEditFile, Arguments: string(arguments),
	}}
}

type contentHashFIFOWriter struct {
	path      string
	content   []byte
	wrote     chan struct{}
	releaseCh chan struct{}
	done      chan error
	once      sync.Once
}

func newContentHashFIFOWriter(t *testing.T, path string, content []byte) *contentHashFIFOWriter {
	t.Helper()
	writer := &contentHashFIFOWriter{
		path:      path,
		content:   content,
		wrote:     make(chan struct{}),
		releaseCh: make(chan struct{}),
		done:      make(chan error, 1),
	}
	t.Cleanup(writer.release)
	go writer.write()
	return writer
}

func (w *contentHashFIFOWriter) write() {
	var file *os.File
	var err error
	for {
		file, err = openContentHashFIFOWriter(w.path)
		if err == nil {
			break
		}
		if !contentHashFIFOUnavailable(err) {
			w.done <- err
			return
		}
		select {
		case <-w.releaseCh:
			w.done <- nil
			return
		case <-time.After(time.Millisecond):
		}
	}
	_, err = file.Write(w.content)
	if err == nil {
		close(w.wrote)
	}
	select {
	case <-w.releaseCh:
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}
	w.done <- err
}

func (w *contentHashFIFOWriter) waitForRead(timeout time.Duration) error {
	select {
	case <-w.wrote:
		return nil
	case err := <-w.done:
		if err == nil {
			return errors.New("FIFO writer stopped before the application opened it")
		}
		return err
	case <-time.After(timeout):
		return errors.New("timed out waiting for application to read FIFO")
	}
}

func (w *contentHashFIFOWriter) release() {
	w.once.Do(func() { close(w.releaseCh) })
}
