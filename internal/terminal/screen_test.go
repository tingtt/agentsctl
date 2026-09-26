package terminal

import (
	"bytes"
	"strings"
	"testing"
)

func TestAlternateScreenLeaveFilterPreservesStreamAcrossArbitraryWrites(t *testing.T) {
	tests := map[string]struct {
		chunks []string
		want   string
	}{
		"single write": {
			chunks: []string{"abc" + AlternateScreenDisable + "xyz"},
			want:   "abcxyz",
		},
		"split sequence": {
			chunks: []string{"abc\x1b[?10", "49", "lxyz"},
			want:   "abcxyz",
		},
		"one byte at a time": {
			chunks: strings.Split("abc"+AlternateScreenDisable+"xyz", ""),
			want:   "abcxyz",
		},
		"near matches": {
			chunks: []string{"a\x1b[?1048lb\x1b[?1049hc\x1b[?1049xd"},
			want:   "a\x1b[?1048lb\x1b[?1049hc\x1b[?1049xd",
		},
		"incomplete final prefix": {
			chunks: []string{"abc\x1b[?10", "4"},
			want:   "abc\x1b[?104",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			filter := NewAlternateScreenLeaveFilter(&out)
			for _, chunk := range tc.chunks {
				if err := filter.Write([]byte(chunk)); err != nil {
					t.Fatal(err)
				}
			}
			if err := filter.Flush(); err != nil {
				t.Fatal(err)
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("filtered output=%q, want %q", got, tc.want)
			}
		})
	}
}
