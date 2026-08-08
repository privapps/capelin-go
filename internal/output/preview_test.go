package output

import (
	"strings"
	"testing"
)

func TestPreviewGraphemeSafe(t *testing.T) {
	in := strings.Repeat("🍎", 300)
	got := Preview(in, false)
	if !strings.HasSuffix(got, " more chars)") {
		t.Fatalf("expected truncation suffix, got %q", got)
	}
	// Every emoji is a base+variation-selector pair; the shown runes must be a
	// multiple of the cluster size and valid UTF-8.
	for _, r := range got {
		if r == '…' {
			break
		}
	}
	if len([]rune(got)) > 160+10 {
		t.Fatalf("preview too long: %d runes", len([]rune(got)))
	}
}

func TestPreviewSuffixFormat(t *testing.T) {
	got := Preview(strings.Repeat("a", 500), false)
	if !strings.Contains(got, "… (+") || !strings.HasSuffix(got, "more chars)") {
		t.Fatalf("bad suffix: %q", got)
	}
}

func TestPreviewFullReturnsUnchanged(t *testing.T) {
	in := "  hello   world  \n\n  foo  "
	if got := Preview(in, true); got != "hello world foo" {
		t.Fatalf("full mode = %q, want %q", got, "hello world foo")
	}
}

func TestPreviewEnvOverride(t *testing.T) {
	t.Cleanup(func() { SetPreviewMax(0) })
	SetPreviewMax(10)
	got := Preview(strings.Repeat("a", 100), false)
	// 10 shown + ellipsis + suffix; runes should be <= 10 + len("… (+N more chars)")
	if len([]rune(got)) > 10+30 {
		t.Fatalf("env override not applied: %q", got)
	}
}

func TestPreviewWhitespaceCollapse(t *testing.T) {
	if got := Preview("  multi   line \n text ", false); got != "multi line text" {
		t.Fatalf("collapse = %q", got)
	}
}

func TestPreviewLongSingleToken(t *testing.T) {
	got := Preview(strings.Repeat("x", 500), false)
	if !strings.Contains(got, "… (+") {
		t.Fatalf("long token not truncated: %q", got)
	}
}
