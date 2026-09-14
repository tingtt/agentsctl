package chatgpt

import (
	"context"
	"fmt"
	"time"
)

const (
	maxPages                  = 100
	maxWheelTicks             = 600
	maxWheelRounds            = 200
	wheelTicksPerRound        = 3
	maxNoProgressRounds       = 6
	initialProjectLoadTimeout = 15 * time.Second
)

type enumerationLimits struct {
	pages          int
	wheelTicks     int
	wheelRounds    int
	ticksPerRound  int
	noProgress     int
	initialTimeout time.Duration
	pollInterval   time.Duration
	settle         time.Duration
	bottomSettle   time.Duration
}

func defaultEnumerationLimits() enumerationLimits {
	return enumerationLimits{
		pages:          maxPages,
		wheelTicks:     maxWheelTicks,
		wheelRounds:    maxWheelRounds,
		ticksPerRound:  wheelTicksPerRound,
		noProgress:     maxNoProgressRounds,
		initialTimeout: initialProjectLoadTimeout,
		pollInterval:   200 * time.Millisecond,
		settle:         400 * time.Millisecond,
		bottomSettle:   800 * time.Millisecond,
	}
}

func enumerate(ctx context.Context, bridge discoveryBridge, projectID string) ([]conversation, error) {
	return enumerateWithLimits(ctx, bridge, projectID, defaultEnumerationLimits())
}

func enumerateWithLimits(ctx context.Context, bridge discoveryBridge, projectID string, limits enumerationLimits) ([]conversation, error) {
	if err := bridge.BeginList(ctx, projectID); err != nil {
		return nil, fmt.Errorf("navigate official ChatGPT Project view: %w", err)
	}

	var selectedSeries string
	var captures []capture
	var region scrollRegion
	var diagnostic initialSeriesDiagnostic
	initialDeadline := time.Now().Add(limits.initialTimeout)
	for selectedSeries == "" {
		var err error
		captures, err = bridge.Captures(ctx, projectID)
		if err != nil {
			return nil, err
		}
		region, err = bridge.ScrollRegion(ctx, projectID)
		if err != nil {
			return nil, err
		}
		diagnostic = diagnoseInitialSeries(captures, region)
		selectedSeries, err = selectInitialSeries(captures, region)
		if err != nil {
			return nil, fmt.Errorf("%w; initial series: %s", err, diagnostic)
		}
		if selectedSeries != "" {
			break
		}
		if time.Now().After(initialDeadline) {
			return nil, fmt.Errorf("no unambiguous Project conversation request series observed within %s; initial series: %s", limits.initialTimeout, diagnostic)
		}
		if err := waitContext(ctx, limits.pollInterval); err != nil {
			return nil, err
		}
	}

	noProgressRounds := 0
	ticksSent := 0
	for round := 0; round < limits.wheelRounds && ticksSent < limits.wheelTicks && noProgressRounds < limits.noProgress; round++ {
		conversations, complete, err := assembleChain(captures, selectedSeries, limits.pages)
		if err != nil {
			return nil, err
		}
		if complete {
			return conversations, nil
		}

		wheel, err := bridge.Wheel(ctx, projectID, limits.ticksPerRound)
		if err != nil {
			return nil, err
		}
		ticksSent += wheel.Ticks
		settle := limits.settle
		if region.Found && distanceToBottom(region) <= region.ClientHeight {
			settle = limits.bottomSettle
		}
		if err := waitContext(ctx, settle); err != nil {
			return nil, err
		}

		nextCaptures, err := bridge.Captures(ctx, projectID)
		if err != nil {
			return nil, err
		}
		nextRegion, err := bridge.ScrollRegion(ctx, projectID)
		if err != nil {
			return nil, err
		}
		progressed := len(nextCaptures) > len(captures) || scrollProgressed(region, nextRegion) || scrollProgressed(wheel.Initial, wheel.Final)
		if progressed {
			noProgressRounds = 0
		} else {
			noProgressRounds++
		}
		captures, region = nextCaptures, nextRegion
	}
	return nil, fmt.Errorf("Project cursor enumeration did not reach an explicit terminal page within defensive bounds")
}

func matchingSeries(captures []capture, projectLinkCount int) []string {
	if projectLinkCount < 0 {
		return nil
	}
	earliest := make(map[string]capture)
	for _, candidate := range captures {
		if candidate.CursorIn != "0" || candidate.SeriesKey == "" {
			continue
		}
		current, ok := earliest[candidate.SeriesKey]
		if !ok || candidate.CaptureID < current.CaptureID {
			earliest[candidate.SeriesKey] = candidate
		}
	}
	var matches []string
	for key, candidate := range earliest {
		if len(candidate.Items) == projectLinkCount {
			matches = append(matches, key)
		}
	}
	return matches
}

func distanceToBottom(region scrollRegion) int {
	distance := region.ScrollHeight - region.ClientHeight - region.ScrollTop
	if distance < 0 {
		return 0
	}
	return distance
}

func scrollProgressed(before, after scrollRegion) bool {
	return before.ScrollTop != after.ScrollTop ||
		before.ScrollHeight != after.ScrollHeight ||
		before.ProjectLinkCount != after.ProjectLinkCount
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
