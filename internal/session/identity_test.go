package session

import (
	"reflect"
	"testing"
)

func TestIdentityTransitions(t *testing.T) {
	k := func(p ProviderID, id string) Key { return Key{Provider: p, ID: id} }
	row := func(key Key, previous ...Key) Session { return Session{Key: key, PreviousKeys: previous} }
	run1, run2 := k(ProviderCodex, "run-1"), k(ProviderCodex, "run-2")
	thread1, thread2 := k(ProviderCodex, "thread-1"), k(ProviderCodex, "thread-2")

	tests := []struct {
		name     string
		sessions []Session
		want     map[Key]Key
	}{
		{name: "no continuity", sessions: []Session{row(thread1), row(thread2)}, want: map[Key]Key{}},
		{name: "unique transition", sessions: []Session{row(thread1, run1)}, want: map[Key]Key{run1: thread1}},
		{
			name:     "several previous keys of one session",
			sessions: []Session{row(thread1, run1, run2)},
			want:     map[Key]Key{run1: thread1, run2: thread1},
		},
		{
			name:     "same previous key listed twice by one row is not ambiguous",
			sessions: []Session{row(thread1, run1, run1)},
			want:     map[Key]Key{run1: thread1},
		},
		{
			name:     "multiple independent valid transitions",
			sessions: []Session{row(thread1, run1), row(thread2, run2)},
			want:     map[Key]Key{run1: thread1, run2: thread2},
		},
		{
			name:     "old key still present",
			sessions: []Session{row(run1), row(thread1, run1)},
			want:     map[Key]Key{},
		},
		{
			name:     "old key still present does not affect other transitions",
			sessions: []Session{row(run1), row(thread1, run1, run2)},
			want:     map[Key]Key{run2: thread1},
		},
		{
			name:     "ambiguous destination",
			sessions: []Session{row(thread1, run1), row(thread2, run1)},
			want:     map[Key]Key{},
		},
		{
			name:     "ambiguous destination is independent of row order",
			sessions: []Session{row(thread2, run1), row(thread1, run1)},
			want:     map[Key]Key{},
		},
		{
			name:     "ambiguity of one key does not block another",
			sessions: []Session{row(thread1, run1, run2), row(thread2, run1)},
			want:     map[Key]Key{run2: thread1},
		},
		{name: "self transition", sessions: []Session{row(thread1, thread1)}, want: map[Key]Key{}},
		{
			name:     "cross-provider transition",
			sessions: []Session{row(thread1, k(ProviderClaude, "abc"))},
			want:     map[Key]Key{},
		},
		{
			name:     "cross-provider claim does not make a valid transition ambiguous",
			sessions: []Session{row(thread1, run1), row(k(ProviderClaude, "c"), run1)},
			want:     map[Key]Key{run1: thread1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IdentityTransitions(tc.sessions); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("IdentityTransitions=%v, want %v", got, tc.want)
			}
		})
	}
}
