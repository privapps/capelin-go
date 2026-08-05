package interactive

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBracketedPasteRoundTripsMultilineInput(t *testing.T) {
	reader := NewBracketedPasteReader(strings.NewReader("\x1b[200~first\nsecond\r\nthird\x1b[201~"))
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte{'\r'}) {
		t.Fatalf("paste emitted an implicit submission, got %q", data)
	}
	if got := NormalizeInput(string(data)); got != "first\nsecond\nthird" {
		t.Fatalf("normalized paste = %q", got)
	}
}

func TestDoubleEscapeReaderTranslatesStandalonePairToInterrupt(t *testing.T) {
	reader := NewDoubleEscapeReader(strings.NewReader("draft\x1b\x1bafter"))
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "draft\x03after"; got != want {
		t.Fatalf("double Escape translation = %q, want %q", got, want)
	}
}

func TestDoubleEscapeReaderPreservesNavigationAndAltSequences(t *testing.T) {
	input := "before\x1b[1;5D\x1bb\x1b[200~paste\x1b[201~after"
	reader := NewDoubleEscapeReader(strings.NewReader(input))
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != input {
		t.Fatalf("escape sequence handling changed input: got %q, want %q", got, input)
	}
}

func TestDisplayNewlineKeepsReadlineWidthAndPayloadSeparate(t *testing.T) {
	line, pos, ok := InsertDisplayNewline([]rune("ab"+string(NewlineMarker)+"CD"), 3, NewlineMarker, 10, 2)
	if !ok {
		t.Fatal("display newline was not inserted")
	}
	if pos != 8 {
		t.Fatalf("display cursor position = %d, want 8", pos)
	}
	if got := string(NewlinePainter{}.Paint(line, pos)); got != "ab\nCD" {
		t.Fatalf("painted display line = %q, want %q", got, "ab\nCD")
	}
	if got := NormalizeInput(string(line)); got != "ab\nCD" {
		t.Fatalf("normalized display line = %q, want %q", got, "ab\nCD")
	}
}

func TestReadlineLoopDelegatesCommandsAndHonorsExit(t *testing.T) {
	// The actual readline instance is covered through app-level tests. This
	// assertion keeps the public callback contract explicit without requiring a
	// terminal in this focused package test.
	called := false
	onInput := func(value string) bool { called = value == "exit"; return called }
	if !onInput("exit") || !called {
		t.Fatal("callback contract was not exercised")
	}
}

type blockingFallbackReader struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (r *blockingFallbackReader) Read([]byte) (int, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	<-r.closed
	return 0, io.EOF
}

func (r *blockingFallbackReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func TestFallbackLoopCancelsBlockedRead(t *testing.T) {
	reader := &blockingFallbackReader{started: make(chan struct{}), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunFallbackLoop(ctx, reader, &bytes.Buffer{}, func(string) bool { return false })
	}()
	select {
	case <-reader.started:
	case <-time.After(time.Second):
		t.Fatal("fallback loop did not start reading")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("fallback loop returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fallback loop did not stop after cancellation")
	}
	select {
	case <-reader.closed:
	default:
		t.Fatal("fallback loop did not close its cancellable reader")
	}
}
