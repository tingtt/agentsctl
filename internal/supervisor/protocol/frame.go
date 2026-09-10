// Package protocol is internal/supervisor's own Unix-socket wire framing:
// a length-prefixed frame kind + payload, and the frame kinds the
// supervisor daemon/client exchange (Request/Response/Input/Output/
// Resize/Detach/Exit/Failure). It is not a generic application protocol --
// nothing outside internal/supervisor speaks it -- so it is nested under
// internal/supervisor rather than kept as a top-level package.
package protocol

import (
	"encoding/binary"
	"errors"
	"io"
)

// TerminalSize is a PTY resize request. Redraw requests a resize notification
// even when Rows and Cols already match the managed PTY.
type TerminalSize struct {
	Rows   uint16 `json:"rows"`
	Cols   uint16 `json:"cols"`
	Redraw bool   `json:"redraw,omitempty"`
}

const MaxFrame = 8 << 20
const (
	Request  byte = 'Q'
	Response byte = 'P'
	Input    byte = 'I'
	Output   byte = 'O'
	Resize   byte = 'R'
	Detach   byte = 'D'
	Exit     byte = 'X'
	Failure  byte = 'E'
)

// Write writes one frame -- a kind byte, a big-endian length, then
// payload -- to w. See WriteFrame for the same operation reporting
// whether the frame was torn, which a caller that might write another
// frame to w afterward must check.
func Write(w io.Writer, kind byte, payload []byte) error {
	_, err := WriteFrame(w, kind, payload)
	return err
}

// WriteFrame is Write, but built as a single w.Write call (header and
// payload concatenated) and reporting torn: whether more than zero, but
// fewer than all, of the frame's bytes reached w. Per io.Writer's
// contract, err == nil implies every byte was written, so torn is only
// ever true alongside a non-nil err.
//
// A torn frame desynchronizes whatever framing state Read expects on the
// other end of w: the peer has already received a truncated header or
// payload with no way to tell it apart from the start of a new one. Once
// WriteFrame reports torn, the caller must never write another frame to
// w -- only close it. A clean failure (torn false) means none of this
// frame's bytes reached the peer, so w is still at the boundary of
// whatever frame came before it, and a further frame can still safely be
// attempted.
func WriteFrame(w io.Writer, kind byte, payload []byte) (torn bool, err error) {
	if len(payload) > MaxFrame {
		return false, errors.New("frame too large")
	}
	frame := make([]byte, 5+len(payload))
	frame[0] = kind
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	n, err := w.Write(frame)
	if err != nil {
		return n > 0 && n < len(frame), err
	}
	return false, nil
}
func Read(r io.Reader) (byte, []byte, error) {
	h := make([]byte, 5)
	if _, err := io.ReadFull(r, h); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxFrame {
		return 0, nil, errors.New("frame too large")
	}
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return h[0], b, err
}
