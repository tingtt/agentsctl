package terminal

import (
	"bytes"
	"io"
)

// Outer-terminal modes an Open transport owns while it forwards a child's
// output to the physical terminal. A transport enters its own alternate
// screen and bracketed-paste mode before forwarding anything and releases
// both after forwarding has stopped, so the user's main screen never
// receives a child frame, whatever state the child leaves behind.
const (
	AlternateScreenEnable  = "\x1b[?1049h"
	AlternateScreenDisable = "\x1b[?1049l"
	BracketedPasteEnable   = "\x1b[?2004h"
	BracketedPasteDisable  = "\x1b[?2004l"
)

// AlternateScreenLeaveFilter removes only a child's DECRST 1049 from the
// physical-terminal stream. The transport owns the outer alternate screen,
// and terminal screen modes are not nested: forwarding a child leave would
// release the transport's screen and expose the user's main buffer. It
// applies only at the final child-to-terminal boundary; the child's own PTY
// state is not touched.
type AlternateScreenLeaveFilter struct {
	out     io.Writer
	pending []byte
}

// NewAlternateScreenLeaveFilter returns a filter writing to out.
func NewAlternateScreenLeaveFilter(out io.Writer) *AlternateScreenLeaveFilter {
	return &AlternateScreenLeaveFilter{out: out}
}

// Write forwards chunk without any complete alternate-screen leave. A
// trailing partial leave is held back until the next Write or Flush.
func (f *AlternateScreenLeaveFilter) Write(chunk []byte) error {
	sequence := []byte(AlternateScreenDisable)
	data := append(f.pending, chunk...)
	f.pending = f.pending[:0]
	for len(data) > 0 {
		if index := bytes.Index(data, sequence); index >= 0 {
			if err := WriteFull(f.out, data[:index]); err != nil {
				return err
			}
			data = data[index+len(sequence):]
			continue
		}
		keep := longestSuffixPrefix(data, sequence)
		if err := WriteFull(f.out, data[:len(data)-keep]); err != nil {
			return err
		}
		f.pending = append(f.pending, data[len(data)-keep:]...)
		return nil
	}
	return nil
}

// Flush writes a held-back partial sequence, which can no longer complete.
func (f *AlternateScreenLeaveFilter) Flush() error {
	err := WriteFull(f.out, f.pending)
	f.pending = f.pending[:0]
	return err
}

func longestSuffixPrefix(data, sequence []byte) int {
	for length := min(len(data), len(sequence)-1); length > 0; length-- {
		if bytes.Equal(data[len(data)-length:], sequence[:length]) {
			return length
		}
	}
	return 0
}

// WriteFull writes all of data to out, treating a short write as an error.
func WriteFull(out io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := out.Write(data)
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
