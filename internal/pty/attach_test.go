package pty

import (
	"bytes"
	"io"
	"testing"
)

func TestDetachIsConsumedAndNeverForwarded(t *testing.T) {
	input := bytes.NewReader([]byte{'a', DetachKey, 'b'})
	var sent bytes.Buffer
	detached := false
	err := forwardInput(input, func(b []byte) error { _, e := sent.Write(b); return e }, func() error { detached = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if sent.String() != "a" || !detached {
		t.Fatalf("sent=%q detached=%v", sent.String(), detached)
	}
	if bytes.Contains(sent.Bytes(), []byte{DetachKey}) {
		t.Fatal("detach key reached child")
	}
}
func TestInputEOFIsReported(t *testing.T) {
	err := forwardInput(bytes.NewReader(nil), func([]byte) error { return nil }, func() error { return nil })
	if err != io.EOF {
		t.Fatalf("err=%v", err)
	}
}
func TestClaudeDetachTranslationUsesNativeControlZ(t *testing.T) {
	sequence := claudeNativeDetachSequence()
	if len(sequence) != 1 || sequence[0] != 0x1a {
		t.Fatalf("sequence=%v", sequence)
	}
}

func TestCtrlSIsForwardedToAttachedChild(t *testing.T) {
	input := bytes.NewReader([]byte{'a', 0x13, 'b', DetachKey})
	var sent bytes.Buffer
	err := forwardInput(input, func(b []byte) error { _, e := sent.Write(b); return e }, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sent.Bytes(), []byte{'a', 0x13, 'b'}) {
		t.Fatalf("sent=%v", sent.Bytes())
	}
}
