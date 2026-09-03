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

func Write(w io.Writer, kind byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return errors.New("frame too large")
	}
	h := make([]byte, 5)
	h[0] = kind
	binary.BigEndian.PutUint32(h[1:], uint32(len(payload)))
	if _, err := w.Write(h); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
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
