package chatgpt

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

var conversationIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f-]{27,}$`)

type conversation struct {
	ID        string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type capturedItem struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

type capture struct {
	CaptureID      int            `json:"captureID"`
	CursorIn       string         `json:"cursorIn"`
	SeriesKey      string         `json:"seriesKey"`
	Items          []capturedItem `json:"items"`
	CursorObserved bool           `json:"cursorObserved"`
	HasNextCursor  bool           `json:"hasNextCursor"`
	NextCursor     string         `json:"nextCursor"`
}

type page struct {
	items         []conversation
	hasNextCursor bool
	nextCursor    string
}

func selectSeries(captures []capture, projectLinkCount int) (string, error) {
	matches := matchingSeries(captures, projectLinkCount)
	if len(matches) != 1 {
		return "", fmt.Errorf("request-series ambiguity: %d series match the Project list's %d links", len(matches), projectLinkCount)
	}
	return matches[0], nil
}

func assembleChain(captures []capture, seriesKey string, maxPages int) ([]conversation, bool, error) {
	if seriesKey == "" {
		return nil, false, fmt.Errorf("request series is not selected")
	}
	if maxPages <= 0 {
		return nil, false, fmt.Errorf("maximum page count must be positive")
	}
	byCursor, err := pagesForSeries(captures, seriesKey)
	if err != nil {
		return nil, false, err
	}
	if len(byCursor) == 0 {
		return nil, false, nil
	}

	seenCursors := make(map[string]bool, len(byCursor))
	byID := make(map[string]conversation)
	cursor := "0"
	for pageIndex := 0; pageIndex < maxPages; pageIndex++ {
		candidate, ok := byCursor[cursor]
		if !ok {
			if len(seenCursors) < len(byCursor) {
				return nil, false, fmt.Errorf("cursor chain broken after %s", redactedCursor(cursor))
			}
			return sortedConversations(byID), false, nil
		}
		if seenCursors[cursor] {
			return nil, false, fmt.Errorf("cursor cycle detected at %s", redactedCursor(cursor))
		}
		seenCursors[cursor] = true
		for _, item := range candidate.items {
			byID[item.ID] = item
		}
		if !candidate.hasNextCursor {
			if len(seenCursors) != len(byCursor) {
				return nil, false, fmt.Errorf("cursor chain contains captures after its terminal page")
			}
			return sortedConversations(byID), true, nil
		}
		if candidate.nextCursor == "" {
			return nil, false, fmt.Errorf("page %d declared an empty next cursor", pageIndex)
		}
		if seenCursors[candidate.nextCursor] {
			return nil, false, fmt.Errorf("cursor cycle detected at %s", redactedCursor(candidate.nextCursor))
		}
		cursor = candidate.nextCursor
	}
	return nil, false, fmt.Errorf("maximum page count %d reached without a terminal cursor", maxPages)
}

func pagesForSeries(captures []capture, seriesKey string) (map[string]page, error) {
	selected := make([]capture, 0, len(captures))
	for _, candidate := range captures {
		if candidate.SeriesKey == seriesKey {
			selected = append(selected, candidate)
		}
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].CaptureID < selected[j].CaptureID })
	byCursor := make(map[string]page, len(selected))
	identitySets := make(map[string]string, len(selected))
	for _, candidate := range selected {
		if candidate.CursorIn == "" {
			return nil, fmt.Errorf("capture %d has an empty request cursor", candidate.CaptureID)
		}
		if !candidate.CursorObserved {
			return nil, fmt.Errorf("capture %d has no explicit response cursor state", candidate.CaptureID)
		}
		items, err := parseItems(candidate.Items)
		if err != nil {
			return nil, fmt.Errorf("capture %d: %w", candidate.CaptureID, err)
		}
		identitySet := conversationIdentitySet(items)
		if previous, exists := identitySets[candidate.CursorIn]; exists && previous != identitySet {
			return nil, fmt.Errorf("cursor %s returned a different conversation set", redactedCursor(candidate.CursorIn))
		}
		identitySets[candidate.CursorIn] = identitySet
		byCursor[candidate.CursorIn] = page{
			items:         items,
			hasNextCursor: candidate.HasNextCursor,
			nextCursor:    candidate.NextCursor,
		}
	}
	return byCursor, nil
}

func parseItems(items []capturedItem) ([]conversation, error) {
	parsed := make([]conversation, 0, len(items))
	for index, item := range items {
		if !conversationIDPattern.MatchString(item.ID) {
			return nil, fmt.Errorf("item %d has no recognizable conversation ID", index)
		}
		if strings.TrimSpace(item.Title) == "" {
			return nil, fmt.Errorf("item %d has no title", index)
		}
		createdAt, err := time.Parse(time.RFC3339Nano, item.CreatedAt)
		if err != nil {
			return nil, fmt.Errorf("item %d has invalid create_time: %w", index, err)
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, item.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("item %d has invalid update_time: %w", index, err)
		}
		parsed = append(parsed, conversation{ID: item.ID, Title: item.Title, CreatedAt: createdAt, UpdatedAt: updatedAt})
	}
	return parsed, nil
}

func conversationIdentitySet(items []conversation) string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	slices.Sort(ids)
	return strings.Join(ids, "\x00")
}

func sortedConversations(byID map[string]conversation) []conversation {
	result := make([]conversation, 0, len(byID))
	for _, item := range byID {
		result = append(result, item)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func redactedCursor(cursor string) string {
	if cursor == "0" {
		return cursor
	}
	sum := sha256.Sum256([]byte(cursor))
	return "cursor:" + hex.EncodeToString(sum[:])[:12]
}
