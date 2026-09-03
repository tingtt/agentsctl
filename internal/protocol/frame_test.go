package protocol

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var b bytes.Buffer
	if err := Write(&b, Output, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	k, p, err := Read(&b)
	if err != nil || k != Output || string(p) != "ok" {
		t.Fatalf("%c %q %v", k, p, err)
	}
}
