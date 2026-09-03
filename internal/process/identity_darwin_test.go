//go:build darwin

package process

import "testing"

func TestCStringStopsAtFirstNUL(t *testing.T) {
	value := []byte{'a', 'g', 'e', 'n', 't', 's', 'c', 't', 'l', 0, 'r', '-', '3', '.', '6'}
	if got := cString(value); got != "agentsctl" {
		t.Fatalf("cString=%q, want agentsctl", got)
	}
}
