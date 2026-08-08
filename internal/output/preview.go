package output

import (
	"errors"
	"strings"
	"unicode"
)

const defaultPreviewMax = 160

// errBadPreviewInt reports an invalid AGENT_QUESTION_PREVIEW_MAX value.
var errBadPreviewInt = errors.New("invalid AGENT_QUESTION_PREVIEW_MAX")

// previewMaxRunes is the total display budget applied by Preview. This package
// owns sinks and rendering, not configuration: the value is wired in by the
// application composition root via SetPreviewMax so env parsing stays in
// internal/config.
var previewMaxRunes = defaultPreviewMax

// SetPreviewMax sets the preview display budget in runes. A non-positive value
// restores the built-in default (160). It is intended to be called once at
// startup by the composition root.
func SetPreviewMax(n int) {
	if n > 0 {
		previewMaxRunes = n
		return
	}
	previewMaxRunes = defaultPreviewMax
}

// isGraphemeExtender reports whether r is a combining mark, enclosing mark,
// zero-width joiner, or emoji variation selector that must stay attached to the
// preceding base rune so a preview never splits a grapheme cluster.
func isGraphemeExtender(r rune) bool {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return true
	}
	switch r {
	case 0x200D, // ZERO WIDTH JOINER
		0xFE0F, // VARIATION SELECTOR-16
		0xFE0E: // VARIATION SELECTOR-15
		return true
	}
	return false
}

// previewSuffixLen returns the rune length of the "… (+N more chars)" suffix
// for a given omitted rune count.
func previewSuffixLen(omitted int) int {
	return len([]rune("… (+" + itoa(omitted) + " more chars)"))
}

// Preview returns a display-safe truncation of text. When full is true the
// collapsed text is returned unchanged. Otherwise it collapses internal
// whitespace and truncates to a cap (default 160 runes, overridable via
// AGENT_QUESTION_PREVIEW_MAX) where the cap is the total display budget
// including the trailing "… (+N more chars)" suffix. A shortened value ends
// with "… (+N more chars)" where N is the omitted rune count, and the cut is
// made on a grapheme boundary so multibyte emoji/CJK are never split.
func Preview(text string, full bool) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	collapsed = strings.TrimSpace(collapsed)
	if full || collapsed == "" {
		return collapsed
	}
	cap := previewMaxRunes
	if cap <= 0 {
		cap = defaultPreviewMax
	}
	runes := []rune(collapsed)
	if len(runes) <= cap {
		return collapsed
	}
	// Reserve space for the suffix so the total preview stays within cap.
	shown := cap
	if shown > len(runes) {
		shown = len(runes)
	}
	for shown > 0 && shown+previewSuffixLen(len(runes)-shown) > cap {
		shown--
	}
	// Never split a grapheme cluster.
	for shown < len(runes) && isGraphemeExtender(runes[shown]) {
		shown++
	}
	omitted := len(runes) - shown
	return string(runes[:shown]) + "… (+" + itoa(omitted) + " more chars)"
}

func parsePreviewInt(s string) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, errBadPreviewInt
		}
		n = n*10 + int(r-'0')
		if n > 1_000_000 {
			return 0, errBadPreviewInt
		}
	}
	if n == 0 {
		return 0, errBadPreviewInt
	}
	return n, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
