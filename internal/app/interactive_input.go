package app

import (
	"capelin-go/internal/interactive"
	"io"
)

// Compatibility aliases keep the application tests and older internal callers
// stable while input ownership lives in the interactive capability module.
const (
	pasteEscape              = interactive.PasteEscape
	pasteStart               = interactive.PasteStart
	pasteEnd                 = interactive.PasteEnd
	pasteLineBreak           = interactive.PasteLineBreak
	interactiveNewlineMarker = interactive.NewlineMarker
)

type modifiedEnterReader = interactive.ModifiedEnterReader
type bracketedPasteReader = interactive.BracketedPasteReader
type interactiveNewlinePainter = interactive.NewlinePainter

func newModifiedEnterReader(reader io.Reader) *modifiedEnterReader {
	return interactive.NewModifiedEnterReader(reader)
}
func newBracketedPasteReader(reader io.Reader) *bracketedPasteReader {
	return interactive.NewBracketedPasteReader(reader)
}
func decodeInteractiveInput(input string) string    { return interactive.DecodeInput(input) }
func normalizeInteractiveInput(input string) string { return interactive.NormalizeInput(input) }
func insertInteractiveNewline(line []rune, pos int, key rune) ([]rune, int, bool) {
	return interactive.InsertNewline(line, pos, key)
}
func newInteractiveNewlineListener(width func() int) func([]rune, int, rune) ([]rune, int, bool) {
	return interactive.NewlineListener(width, 2)
}
func bracketedPasteSupported() bool { return interactive.BracketedPasteSupported() }
func enableBracketedPaste() func()  { return interactive.EnableBracketedPaste() }
