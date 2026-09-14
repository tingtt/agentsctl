package chatgpt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeDiscoveryBridge struct {
	snapshots    [][]capture
	regions      []scrollRegion
	wheelIndex   int
	wheelCalls   int
	closed       bool
	beginListErr error
}

type slowGrowingBridge struct {
	pages      []capture
	pageIndex  int
	wheelCalls int
}

type settlingSelectionBridge struct {
	captures []capture
	checks   int
}

func (b *settlingSelectionBridge) BeginList(context.Context, string) error { return nil }
func (b *settlingSelectionBridge) Captures(context.Context, string) ([]capture, error) {
	b.checks++
	return b.captures, nil
}
func (b *settlingSelectionBridge) ScrollRegion(context.Context, string) (scrollRegion, error) {
	links := 7
	if b.checks > 1 {
		links = 10
	}
	return scrollRegion{Found: true, ProjectLinkCount: links}, nil
}
func (b *settlingSelectionBridge) Wheel(context.Context, string, int) (wheelResult, error) {
	return wheelResult{}, errors.New("wheel should not be called")
}
func (b *settlingSelectionBridge) Close() error { return nil }

func (b *slowGrowingBridge) BeginList(context.Context, string) error { return nil }
func (b *slowGrowingBridge) Captures(context.Context, string) ([]capture, error) {
	return b.pages[:b.pageIndex+1], nil
}
func (b *slowGrowingBridge) ScrollRegion(context.Context, string) (scrollRegion, error) {
	return b.region(), nil
}
func (b *slowGrowingBridge) Wheel(_ context.Context, _ string, ticks int) (wheelResult, error) {
	initial := b.region()
	b.wheelCalls++
	if b.wheelCalls%13 == 0 && b.pageIndex+1 < len(b.pages) {
		b.pageIndex++
	}
	return wheelResult{Found: true, Initial: initial, Final: b.region(), Ticks: ticks}, nil
}
func (b *slowGrowingBridge) Close() error { return nil }
func (b *slowGrowingBridge) region() scrollRegion {
	return scrollRegion{
		Found:            true,
		ProjectLinkCount: (b.pageIndex + 1) * 10,
		ScrollTop:        b.wheelCalls * 20,
		ScrollHeight:     1000 + b.pageIndex*500 + b.wheelCalls*20,
		ClientHeight:     500,
	}
}

func (f *fakeDiscoveryBridge) BeginList(context.Context, string) error { return f.beginListErr }
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

func TestEnumerateWaitsForMultipleSeriesDOMSignalToSettle(t *testing.T) {
	items := make([]capturedItem, 10)
	for index := range items {
		items[index] = item(conversationForIndex(index), "conversation", "2026-01-01T00:00:00Z")
	}
	bridge := &settlingSelectionBridge{captures: []capture{
		capturePage(1, "small", "0", "", items[:5]...),
		capturePage(2, "project", "0", "", items...),
	}}
	limits := testEnumerationLimits()
	limits.pollInterval = 0
	got, err := enumerateWithLimits(context.Background(), bridge, "g-p-test", limits)
	if err != nil || len(got) != len(items) || bridge.checks != 2 {
		t.Fatalf("conversations=%d checks=%d err=%v", len(got), bridge.checks, err)
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

func TestEnumerateAllowsLargeGrowingProjectWithinDefensiveBounds(t *testing.T) {
	pages := make([]capture, 6)
	for index := range pages {
		cursorIn := "0"
		if index > 0 {
			cursorIn = fmt.Sprintf("cursor-%d", index)
		}
		next := ""
		if index+1 < len(pages) {
			next = fmt.Sprintf("cursor-%d", index+1)
		}
		pages[index] = capturePage(index+1, "project", cursorIn, next, item(conversationForIndex(index), "conversation", "2026-01-01T00:00:00Z"))
	}
	bridge := &slowGrowingBridge{pages: pages}
	limits := defaultEnumerationLimits()
	limits.initialTimeout = time.Second
	limits.pollInterval = 0
	limits.settle = 0
	limits.bottomSettle = 0
	got, err := enumerateWithLimits(context.Background(), bridge, "g-p-test", limits)
	if err != nil || len(got) != len(pages) {
		t.Fatalf("conversations=%d wheelCalls=%d err=%v", len(got), bridge.wheelCalls, err)
	}
	if bridge.wheelCalls*limits.ticksPerRound <= 60 {
		t.Fatalf("fixture used %d ticks; it must exceed the old 60-tick ceiling", bridge.wheelCalls*limits.ticksPerRound)
	}
}

func conversationForIndex(index int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012d", index+1)
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
