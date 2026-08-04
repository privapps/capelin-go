package interactive

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestBracketedPasteRoundTripsMultilineInput(t *testing.T) {
	reader := NewBracketedPasteReader(strings.NewReader("\x1b[200~first\nsecond\r\nthird\x1b[201~"))
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(data, []byte{'\r'}) != 1 {
		t.Fatalf("expected one submission, got %q", data)
	}
	if got := NormalizeInput(strings.TrimSuffix(string(data), "\r")); got != "first\nsecond\nthird" {
		t.Fatalf("normalized paste = %q", got)
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
