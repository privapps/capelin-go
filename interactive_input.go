package main

import (
	"bytes"
	"io"
	"runtime"
	"strings"

	"github.com/chzyer/readline"
)

// The readline package treats LF and CR as submission keys. These control
// bytes are ordinary input to readline, so they let a paste retain its line
// structure without creating additional submissions. They are deliberately
// escaped inside paste payloads before being written to readline's history.
const (
	pasteEscape    byte = 0x1c
	pasteStart     byte = 0x1d
	pasteEnd       byte = 0x1e
	pasteLineBreak byte = 0x1f
)

var (
	bracketedPasteStart = []byte("\x1b[200~")
	bracketedPasteEnd   = []byte("\x1b[201~")
)

// bracketedPasteReader removes terminal bracketed-paste delimiters, replaces
// line breaks in the paste with readline-safe runes, and injects one Enter at
// the end of each paste. Ordinary input is passed through unchanged.
//
// The reader is only installed for a Unix-like TTY. Keeping this adapter
// outside readline also means the existing non-TTY and fallback paths retain
// their line-oriented behavior.
type bracketedPasteReader struct {
	reader io.Reader

	output    []byte
	candidate []byte
	inPaste   bool
	pendingCR bool
	err       error
	readBuf   [4096]byte
}

func newBracketedPasteReader(reader io.Reader) *bracketedPasteReader {
	return &bracketedPasteReader{reader: reader}
}

func (r *bracketedPasteReader) Close() error {
	if closer, ok := r.reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (r *bracketedPasteReader) Read(p []byte) (int, error) {
	for len(r.output) == 0 && r.err == nil {
		n, err := r.reader.Read(r.readBuf[:])
		for _, b := range r.readBuf[:n] {
			r.feed(b)
		}
		if err != nil {
			r.flushPending()
			r.err = err
		}
	}

	if len(r.output) > 0 {
		n := copy(p, r.output)
		r.output = r.output[n:]
		return n, nil
	}
	return 0, r.err
}

func (r *bracketedPasteReader) feed(b byte) {
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
				r.output = append(r.output, pasteEnd, '\r')
			} else {
				r.inPaste = true
				r.output = append(r.output, pasteStart)
			}
			return
		}

		// The candidate is not a delimiter. Release its first byte as
		// payload and reconsider the remaining bytes; this preserves an
		// unrelated escape sequence, including one split across reads.
		first := r.candidate[0]
		r.candidate = r.candidate[1:]
		r.appendPayloadByte(first)
	}
}

func (r *bracketedPasteReader) appendPasteLineBreak() {
	// A doubled line-break marker distinguishes an encoded LF from a
	// literal pasteLineBreak byte escaped by appendPayloadByte.
	r.output = append(r.output, pasteEscape, pasteLineBreak, pasteLineBreak)
}

func (r *bracketedPasteReader) appendPayloadByte(b byte) {
	if r.inPaste {
		switch b {
		case pasteEscape, pasteStart, pasteEnd, pasteLineBreak:
			r.output = append(r.output, pasteEscape)
		}
	}
	r.output = append(r.output, b)
}

func (r *bracketedPasteReader) flushPending() {
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

// decodeInteractiveInput reverses the encoding used by
// bracketedPasteReader. It accepts an incomplete envelope as well so an EOF
// during a paste does not leak the internal control bytes to the model.
func decodeInteractiveInput(input string) string {
	var out bytes.Buffer
	inPaste := false
	data := []byte(input)
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case pasteStart:
			inPaste = true
		case pasteEnd:
			if inPaste {
				inPaste = false
			} else {
				out.WriteByte(data[i])
			}
		case pasteEscape:
			if inPaste && i+1 < len(data) {
				i++
				if data[i] == pasteLineBreak && i+1 < len(data) && data[i+1] == pasteLineBreak {
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
	return out.String()
}

func normalizeInteractiveInput(input string) string {
	return strings.TrimSpace(decodeInteractiveInput(input))
}

func bracketedPasteSupported() bool {
	return runtime.GOOS != "windows" && readline.DefaultIsTerminal()
}

func enableBracketedPaste() func() {
	if !bracketedPasteSupported() {
		return func() {}
	}
	_, _ = io.WriteString(readline.Stderr, "\x1b[?2004h")
	return func() {
		_, _ = io.WriteString(readline.Stderr, "\x1b[?2004l")
	}
}
