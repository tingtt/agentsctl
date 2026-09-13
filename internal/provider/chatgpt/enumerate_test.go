package chatgpt

import (
	"context"
	"strings"
	"testing"
	"time"
)

type fakeDiscoveryBridge struct {
	snapshots  [][]capture
	regions    []scrollRegion
	wheelIndex int
	wheelCalls int
	closed     bool
}

func (f *fakeDiscoveryBridge) BeginList(context.Context, string) error { return nil }
func (f *fakeDiscoveryBridge) Captures(context.Context, string) ([]capture, error) {
	return f.snapshots[min(f.wheelIndex, len(f.snapshots)-1)], nil
}
func (f *fakeDiscoveryBridge) ScrollRegion(context.Context, string) (scrollRegion, error) {
	return f.regions[min(f.wheelIndex, len(f.regions)-1)], nil
}
func (f *fakeDiscoveryBridge) Wheel(_ context.Context, _ string, ticks int) (wheelResult, error) {
	before := f.regions[min(f.wheelIndex, len(f.regions)-1)]
	f.wheelCalls++
	if f.wheelIndex+1 < len(f.snapshots) {
		f.wheelIndex++
	}
	after := f.regions[min(f.wheelIndex, len(f.regions)-1)]
	return wheelResult{Found: true, Initial: before, Final: after, Ticks: ticks}, nil
}
func (f *fakeDiscoveryBridge) Close() error { f.closed = true; return nil }

func TestEnumerateTracksGrowingBottomUntilTerminalCursor(t *testing.T) {
	first := capturePage(1, "project", "0", "opaque", item(conversationA, "A", "2026-01-01T00:00:00Z"))
	terminal := capturePage(2, "project", "opaque", "", item(conversationB, "B", "2026-02-01T00:00:00Z"))
	bridge := &fakeDiscoveryBridge{
		snapshots: [][]capture{{first}, {first, terminal}},
		regions: []scrollRegion{
			{Found: true, ProjectLinkCount: 1, ScrollTop: 400, ScrollHeight: 1000, ClientHeight: 500},
			{Found: true, ProjectLinkCount: 2, ScrollTop: 900, ScrollHeight: 1600, ClientHeight: 500},
		},
	}
	got, err := enumerateWithLimits(context.Background(), bridge, "g-p-test", testEnumerationLimits())
	if err != nil || len(got) != 2 || bridge.wheelCalls != 1 {
		t.Fatalf("conversations=%+v wheelCalls=%d err=%v", got, bridge.wheelCalls, err)
	}
	if bridge.regions[1].ScrollHeight <= bridge.regions[0].ScrollHeight {
		t.Fatal("fixture must prove the current bottom moved after pagination")
	}
}

func TestEnumerateReturnsErrorInsteadOfPartialListWithoutTerminalCursor(t *testing.T) {
	first := capturePage(1, "project", "0", "opaque", item(conversationA, "A", "2026-01-01T00:00:00Z"))
	bridge := &fakeDiscoveryBridge{
		snapshots: [][]capture{{first}},
		regions:   []scrollRegion{{Found: true, ProjectLinkCount: 1, ScrollHeight: 1000, ClientHeight: 500}},
	}
	limits := testEnumerationLimits()
	limits.wheelRounds = 1
	limits.wheelTicks = 3
	if got, err := enumerateWithLimits(context.Background(), bridge, "g-p-test", limits); err == nil || got != nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("conversations=%+v err=%v", got, err)
	}
}

func testEnumerationLimits() enumerationLimits {
	return enumerationLimits{
		pages:          10,
		wheelTicks:     12,
		wheelRounds:    4,
		ticksPerRound:  3,
		noProgress:     2,
		initialTimeout: time.Second,
		pollInterval:   time.Millisecond,
		settle:         0,
		bottomSettle:   0,
	}
}
