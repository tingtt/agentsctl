package protocol

import (
	"bytes"
	"errors"
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

// scriptedWriter is a deterministic io.Writer whose single Write call
// returns a fixed (n, err), so WriteFrame's torn detection can be tested
// against exact byte offsets instead of real transport timing.
type scriptedWriter struct {
	n   int
	err error
}

func (w *scriptedWriter) Write(p []byte) (int, error) {
	n := w.n
	if n > len(p) {
		n = len(p)
	}
	return n, w.err
}

// TestWriteFrameReportsTornOnlyWhenPartiallyDelivered fixes the exact
// boundary WriteFrame's callers rely on to decide whether writing another
// frame to the same connection is still safe: torn must be true whenever
// some but not all of the frame's bytes reached the writer, and false
// both on full success and on a clean failure that sent nothing at all.
func TestWriteFrameReportsTornOnlyWhenPartiallyDelivered(t *testing.T) {
	boom := errors.New("boom")
	payload := []byte("hello") // frame length = 5 (header) + 5 (payload) = 10
	const frameLen = 10

	cases := []struct {
		name     string
		n        int
		err      error
		wantTorn bool
	}{
		{"full success", frameLen, nil, false},
		{"clean failure, nothing sent", 0, boom, false},
		{"torn mid-header", 2, boom, true},
		{"torn exactly at header boundary", 5, boom, true},
		{"torn mid-payload", 7, boom, true},
		{"torn one byte short of complete", frameLen - 1, boom, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &scriptedWriter{n: tc.n, err: tc.err}
			torn, err := WriteFrame(w, Output, payload)
			if torn != tc.wantTorn {
				t.Fatalf("torn=%v, want %v", torn, tc.wantTorn)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("err=%v, want %v", err, tc.err)
			}
		})
	}
}
