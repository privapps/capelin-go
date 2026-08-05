// Package interactive owns terminal input normalization and the small loop seam
// between readline/fallback input and application command handling. It has no
// dependency on the application composition root.
package interactive

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"strings"
	"unicode"

	"github.com/chzyer/readline"
)

const (
	PasteEscape    byte = 0x1c
	PasteStart     byte = 0x1d
	PasteEnd       byte = 0x1e
	PasteLineBreak byte = 0x1f
	NewlineMarker  rune = '\ue000'
	// RenderedNewlineMarker and NewlinePaddingMarker are readline-internal
	// representations. Readline v1.5.1 calculates the current screen row from
	// rune widths but does not treat '\n' as a line boundary. The padding makes
	// readline's row calculation agree with the physical newline emitted by the
	// painter below.
	RenderedNewlineMarker rune = '\ue001'
	NewlinePaddingMarker  rune = '\ue002'
)

var (
	bracketedPasteStart    = []byte("\x1b[200~")
	bracketedPasteEnd      = []byte("\x1b[201~")
	modifiedEnterSequences = [][]byte{
		[]byte("\x1b[13;2u"), []byte("\x1b[13;3u"), []byte("\x1b[13;4u"),
		[]byte("\x1b[13;5u"), []byte("\x1b[13;6u"), []byte("\x1b[13;7u"),
		[]byte("\x1b[13;8u"), []byte("\x1b[13;2~"), []byte("\x1b[13;3~"),
		[]byte("\x1b[13;4~"), []byte("\x1b[13;5~"), []byte("\x1b[13;6~"),
		[]byte("\x1b[13;7~"), []byte("\x1b[13;8~"), []byte("\x1b[27;2;13~"),
		[]byte("\x1b[27;3;13~"), []byte("\x1b[27;4;13~"), []byte("\x1b[27;5;13~"),
		[]byte("\x1b[27;6;13~"), []byte("\x1b[27;7;13~"), []byte("\x1b[27;8;13~"),
	}
)

type ModifiedEnterReader struct {
	reader            io.Reader
	output, candidate []byte
	err               error
	readBuf           [4096]byte
}

// DoubleEscapeReader translates two adjacent standalone Escape bytes into a
// Ctrl+C byte. Escape-prefixed navigation and Alt-key sequences are passed
// through unchanged because their second byte is not another Escape.
type DoubleEscapeReader struct {
	reader            io.Reader
	output, candidate []byte
	err               error
	readBuf           [4096]byte
}

func NewDoubleEscapeReader(reader io.Reader) *DoubleEscapeReader {
	return &DoubleEscapeReader{reader: reader}
}

func (r *DoubleEscapeReader) Close() error {
	if closer, ok := r.reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (r *DoubleEscapeReader) Read(p []byte) (int, error) {
	for len(r.output) == 0 && r.err == nil {
		n, err := r.reader.Read(r.readBuf[:])
		for _, b := range r.readBuf[:n] {
			r.feed(b)
		}
		if err != nil {
			if len(r.candidate) > 0 {
				r.output = append(r.output, '\x1b')
				r.candidate = r.candidate[:0]
			}
			r.err = err
		}
		if n == 0 && err == nil {
			return 0, nil
		}
	}
	if len(r.output) > 0 {
		n := copy(p, r.output)
		r.output = r.output[n:]
		return n, nil
	}
	return 0, r.err
}

func (r *DoubleEscapeReader) feed(b byte) {
	if len(r.candidate) == 0 {
		if b == '\x1b' {
			r.candidate = append(r.candidate, b)
			return
		}
		r.output = append(r.output, b)
		return
	}
	if b == '\x1b' {
		r.output = append(r.output, '\x03')
		r.candidate = r.candidate[:0]
		return
	}
	r.output = append(r.output, '\x1b', b)
	r.candidate = r.candidate[:0]
}

func NewModifiedEnterReader(reader io.Reader) *ModifiedEnterReader {
	return &ModifiedEnterReader{reader: reader}
}
func (r *ModifiedEnterReader) Close() error {
	if closer, ok := r.reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
func (r *ModifiedEnterReader) Read(p []byte) (int, error) {
	for len(r.output) == 0 && r.err == nil {
		n, err := r.reader.Read(r.readBuf[:])
		for _, b := range r.readBuf[:n] {
			r.feed(b)
		}
		if err != nil {
			r.flushCandidate()
			r.err = err
		}
		if n == 0 && err == nil {
			return 0, nil
		}
	}
	if len(r.output) > 0 {
		n := copy(p, r.output)
		r.output = r.output[n:]
		return n, nil
	}
	return 0, r.err
}
func (r *ModifiedEnterReader) feed(b byte) {
	if len(r.candidate) == 0 && b != '\x1b' {
		if b == '\n' {
			r.output = append(r.output, string(NewlineMarker)...)
		} else {
			r.output = append(r.output, b)
		}
		return
	}
	r.candidate = append(r.candidate, b)
	for len(r.candidate) > 0 {
		if isModifiedEnterSequence(r.candidate) {
			r.output = append(r.output, string(NewlineMarker)...)
			r.candidate = r.candidate[:0]
			return
		}
		if isModifiedEnterPrefix(r.candidate) {
			return
		}
		r.output = append(r.output, r.candidate[0])
		r.candidate = r.candidate[1:]
	}
}
func (r *ModifiedEnterReader) flushCandidate() {
	r.output = append(r.output, r.candidate...)
	r.candidate = r.candidate[:0]
}
func isModifiedEnterSequence(candidate []byte) bool {
	for _, sequence := range modifiedEnterSequences {
		if bytes.Equal(candidate, sequence) {
			return true
		}
	}
	return false
}
func isModifiedEnterPrefix(candidate []byte) bool {
	for _, sequence := range modifiedEnterSequences {
		if len(candidate) < len(sequence) && bytes.HasPrefix(sequence, candidate) {
			return true
		}
	}
	return false
}

type BracketedPasteReader struct {
	reader             io.Reader
	output, candidate  []byte
	inPaste, pendingCR bool
	err                error
	readBuf            [4096]byte
}

func NewBracketedPasteReader(reader io.Reader) *BracketedPasteReader {
	return &BracketedPasteReader{reader: reader}
}
func (r *BracketedPasteReader) Close() error {
	if closer, ok := r.reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
func (r *BracketedPasteReader) Read(p []byte) (int, error) {
	for len(r.output) == 0 && r.err == nil {
		n, err := r.reader.Read(r.readBuf[:])
		for _, b := range r.readBuf[:n] {
			r.feed(b)
		}
		if err != nil {
			r.flushPending()
			r.err = err
		}
		if n == 0 && err == nil {
			return 0, nil
		}
	}
	if len(r.output) > 0 {
		n := copy(p, r.output)
		r.output = r.output[n:]
		return n, nil
	}
	return 0, r.err
}
func (r *BracketedPasteReader) feed(b byte) {
	if r.pendingCR {
		r.pendingCR = false
		if b == '\n' {
			r.appendPasteLineBreak()
			return
		}
		r.appendPasteLineBreak()
	}
	if b == '\r' && r.inPaste {
		r.pendingCR = true
		return
	}
	if b == '\n' && r.inPaste {
		r.appendPasteLineBreak()
		return
	}
	if len(r.candidate) == 0 && b != '\x1b' {
		r.appendPayloadByte(b)
		return
	}
	r.candidate = append(r.candidate, b)
	for len(r.candidate) > 0 {
		marker := bracketedPasteStart
		if r.inPaste {
			marker = bracketedPasteEnd
		}
		if bytes.HasPrefix(marker, r.candidate) {
			if len(r.candidate) < len(marker) {
				return
			}
			r.candidate = r.candidate[:0]
			if r.inPaste {
				r.inPaste = false
				r.output = append(r.output, PasteEnd)
			} else {
				r.inPaste = true
				r.output = append(r.output, PasteStart)
			}
			return
		}
		first := r.candidate[0]
		r.candidate = r.candidate[1:]
		r.appendPayloadByte(first)
	}
}
func (r *BracketedPasteReader) appendPasteLineBreak() {
	r.output = append(r.output, PasteEscape, PasteLineBreak, PasteLineBreak)
}
func (r *BracketedPasteReader) appendPayloadByte(b byte) {
	if r.inPaste {
		switch b {
		case PasteEscape, PasteStart, PasteEnd, PasteLineBreak:
			r.output = append(r.output, PasteEscape)
		}
	}
	r.output = append(r.output, b)
}
func (r *BracketedPasteReader) flushPending() {
	if r.pendingCR {
		r.pendingCR = false
		r.appendPasteLineBreak()
	}
	for len(r.candidate) > 0 {
		first := r.candidate[0]
		r.candidate = r.candidate[1:]
		r.appendPayloadByte(first)
	}
}

func DecodeInput(input string) string {
	var out bytes.Buffer
	inPaste := false
	data := []byte(input)
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case PasteStart:
			inPaste = true
		case PasteEnd:
			if inPaste {
				inPaste = false
			} else {
				out.WriteByte(data[i])
			}
		case PasteEscape:
			if inPaste && i+1 < len(data) {
				i++
				if data[i] == PasteLineBreak && i+1 < len(data) && data[i+1] == PasteLineBreak {
					out.WriteByte('\n')
					i++
				} else {
					out.WriteByte(data[i])
				}
			} else {
				out.WriteByte(data[i])
			}
		default:
			out.WriteByte(data[i])
		}
	}
	return strings.NewReplacer(
		string(NewlineMarker), "\n",
		string(RenderedNewlineMarker), "\n",
		string(NewlinePaddingMarker), "",
	).Replace(out.String())
}
func NormalizeInput(input string) string { return strings.TrimSpace(DecodeInput(input)) }

// InsertNewline inserts a logical newline at the marker's cursor position.
// It is kept as the small, display-independent seam used by callers that do
// not have a terminal width available.
func InsertNewline(line []rune, pos int, key rune) ([]rune, int, bool) {
	if key != NewlineMarker {
		return nil, 0, false
	}
	if pos < 0 {
		pos = 0
	}
	if pos > len(line) {
		pos = len(line)
	}
	markerPos := pos
	if markerPos > 0 && line[markerPos-1] == NewlineMarker {
		markerPos--
	}
	updated := make([]rune, 0, len(line))
	updated = append(updated, line[:markerPos]...)
	updated = append(updated, '\n')
	updated = append(updated, line[pos:]...)
	return updated, markerPos + 1, true
}

// InsertDisplayNewline replaces an input marker with a renderable newline and
// invisible width-padding. readline's renderer does not understand embedded
// newlines when deciding which rows to clear, so the padding forces its
// internal cursor calculation to wrap exactly where the terminal does.
func InsertDisplayNewline(line []rune, pos int, key rune, width, promptWidth int) ([]rune, int, bool) {
	if key != NewlineMarker {
		return nil, 0, false
	}
	if pos < 0 {
		pos = 0
	}
	if pos > len(line) {
		pos = len(line)
	}
	markerPos := pos
	if markerPos == 0 || line[markerPos-1] != NewlineMarker {
		return nil, 0, false
	}
	markerPos--
	if width <= 0 {
		width = 80
	}
	if promptWidth < 0 {
		promptWidth = 0
	}

	column := displayColumn(line[:markerPos], width, promptWidth)
	padding := width - column - 1
	if padding < 0 {
		padding = 0
	}
	updated := make([]rune, 0, len(line)+padding)
	updated = append(updated, line[:markerPos]...)
	updated = append(updated, RenderedNewlineMarker)
	for i := 0; i < padding; i++ {
		updated = append(updated, NewlinePaddingMarker)
	}
	updated = append(updated, line[pos:]...)
	return updated, markerPos + 1 + padding, true
}

func displayColumn(line []rune, width, promptWidth int) int {
	column := promptWidth
	for _, r := range line {
		column += displayRuneWidth(r)
		if column >= width {
			column = 0
		}
	}
	return column
}

func displayRuneWidth(r rune) int {
	if r == '\t' {
		return 4
	}
	if unicode.IsOneOf([]*unicode.RangeTable{unicode.Mn, unicode.Me, unicode.Cc, unicode.Cf}, r) {
		return 0
	}
	if unicode.IsOneOf([]*unicode.RangeTable{unicode.Han, unicode.Hangul, unicode.Hiragana, unicode.Katakana}, r) {
		return 2
	}
	return 1
}

// NewlinePainter renders the internal newline representation as a physical
// line break while omitting its invisible width-padding and transient input
// marker.
type NewlinePainter struct{}

func (NewlinePainter) Paint(line []rune, _ int) []rune {
	painted := make([]rune, 0, len(line))
	for _, r := range line {
		switch r {
		case NewlineMarker, NewlinePaddingMarker:
			continue
		case RenderedNewlineMarker:
			painted = append(painted, '\n')
		default:
			painted = append(painted, r)
		}
	}
	return painted
}

// NewlineListener returns a readline listener configured for the terminal's
// current width. width is evaluated only when Ctrl-J is pressed so terminal
// resizes are reflected without rebuilding the readline instance.
func NewlineListener(width func() int, promptWidth int) func([]rune, int, rune) ([]rune, int, bool) {
	return func(line []rune, pos int, key rune) ([]rune, int, bool) {
		currentWidth := 0
		if width != nil {
			currentWidth = width()
		}
		return InsertDisplayNewline(line, pos, key, currentWidth, promptWidth)
	}
}
func BracketedPasteSupported() bool { return runtime.GOOS != "windows" && readline.DefaultIsTerminal() }
func EnableBracketedPaste() func() {
	if !BracketedPasteSupported() {
		return func() {}
	}
	_, _ = io.WriteString(readline.Stderr, "\x1b[?2004h")
	return func() { _, _ = io.WriteString(readline.Stderr, "\x1b[?2004l") }
}

// ReadlineLoopHooks owns EOF, interrupt, cancellation, and command callback
// behavior. The application supplies the callbacks so this package remains
// independent from sessions, agents, tools, and output sinks.
type ReadlineLoopHooks struct {
	BeforeRead  func()
	OnInput     func(string) bool
	OnInterrupt func(string) bool
}

// RunReadlineLoop preserves the original loop seam for callers that only need
// ordinary input handling.
func RunReadlineLoop(ctx context.Context, rl *readline.Instance, onInput func(string) bool) error {
	return RunReadlineLoopWithHooks(ctx, rl, ReadlineLoopHooks{OnInput: onInput})
}

// RunReadlineLoopWithHooks lets an application handle interrupts without
// making a busy asynchronous turn look like an instruction to exit the REPL.
func RunReadlineLoopWithHooks(ctx context.Context, rl *readline.Instance, hooks ReadlineLoopHooks) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if hooks.BeforeRead != nil {
			hooks.BeforeRead()
		}
		line, err := rl.Readline()
		if err == readline.ErrInterrupt {
			if hooks.OnInterrupt != nil {
				if hooks.OnInterrupt(line) {
					break
				}
				continue
			}
			// Ctrl+C on a non-empty line clears it and reprompts. Ctrl+C on
			// an empty line exits, matching readline's existing contract.
			if strings.TrimSpace(line) == "" {
				break
			}
			continue
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hooks.OnInput == nil {
			continue
		}
		if hooks.OnInput(line) {
			break
		}
	}
	return nil
}

// RunFallbackLoop is the non-readline seam used on unsupported terminals.
func RunFallbackLoop(ctx context.Context, reader io.Reader, prompt io.Writer, onInput func(string) bool) error {
	buffer := make([]byte, 4096)
	pending := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if _, err := io.WriteString(prompt, "\n> "); err != nil {
			return err
		}
		type readResult struct {
			n   int
			err error
		}
		readDone := make(chan readResult, 1)
		go func() {
			n, err := reader.Read(buffer)
			readDone <- readResult{n: n, err: err}
		}()
		var result readResult
		select {
		case result = <-readDone:
		case <-ctx.Done():
			if closer, ok := reader.(io.Closer); ok {
				_ = closer.Close()
			}
			return nil
		}
		n, err := result.n, result.err
		if n > 0 {
			pending += string(buffer[:n])
			for {
				index := strings.IndexByte(pending, '\n')
				if index < 0 {
					break
				}
				line := pending[:index]
				pending = pending[index+1:]
				if onInput(line) {
					return nil
				}
				if ctx.Err() != nil {
					return nil
				}
			}
		}
		if err != nil {
			if pending != "" {
				_ = onInput(pending)
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
