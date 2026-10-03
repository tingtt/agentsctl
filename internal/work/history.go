package work

import (
	"fmt"
	"sort"
	"strconv"
)

type promptEntry struct {
	comment Comment
	prompt  Prompt
}

// Reconstruct derives Work history solely from an Issue's current comments.
// Its result is independent of fetched comment ordering.
func Reconstruct(comments []Comment) History {
	ordered := append([]Comment(nil), comments...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	prompts := make(map[CommentID]promptEntry)
	var history History
	for _, comment := range ordered {
		parsed := Parse(comment.Body)
		switch parsed.Kind {
		case ValidPrompt:
			prompts[comment.ID] = promptEntry{comment: comment, prompt: *parsed.Prompt}
			if !comment.Trust.Trusted {
				history.Warnings = append(history.Warnings, warning(comment, WarningUntrusted, string(comment.Trust.Reason)))
			}
		case ValidStatus:
			item := StatusHistory{Comment: comment, Status: *parsed.Status, Authoritative: comment.Trust.Trusted}
			history.Statuses = append(history.Statuses, item)
			if !comment.Trust.Trusted {
				history.Warnings = append(history.Warnings, warning(comment, WarningUntrusted, string(comment.Trust.Reason)))
			}
		case UnsupportedProtocol:
			history.Warnings = append(history.Warnings, warning(comment, WarningUnsupported, fmt.Sprintf("%s version %d", parsed.Type, parsed.Version)))
			if !comment.Trust.Trusted {
				history.Warnings = append(history.Warnings, warning(comment, WarningUntrusted, string(comment.Trust.Reason)))
			}
			if parsed.Type == "status" && comment.Trust.Trusted {
				if id, err := parseCommentID(parsed.Fields["prompt"]); err == nil {
					history.UnsupportedStatuses = append(history.UnsupportedStatuses, UnsupportedStatus{Comment: comment, Prompt: id, Version: parsed.Version})
				}
			}
		case UnknownProtocol:
			history.Warnings = append(history.Warnings, warning(comment, WarningUnknownType, parsed.Type))
			if !comment.Trust.Trusted {
				history.Warnings = append(history.Warnings, warning(comment, WarningUntrusted, string(comment.Trust.Reason)))
			}
		case MalformedProtocol:
			history.Warnings = append(history.Warnings, warning(comment, WarningMalformed, parsed.Err.Error()))
		}
	}

	for i := range history.Statuses {
		status := &history.Statuses[i]
		if _, ok := prompts[status.Status.Prompt]; !ok {
			status.Orphan = true
			status.Authoritative = false
			history.Warnings = append(history.Warnings, warning(status.Comment, WarningOrphanStatus, strconv.FormatInt(int64(status.Status.Prompt), 10)))
		}
	}

	canonical := canonicalContinuations(prompts, history.Statuses)
	ids := make([]CommentID, 0, len(prompts))
	for id := range prompts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		entry := prompts[id]
		item := PromptHistory{Comment: entry.comment, Prompt: entry.prompt, AutoTrusted: entry.comment.Trust.Trusted}
		for _, status := range history.Statuses {
			if status.Status.Prompt == id {
				item.Statuses = append(item.Statuses, status)
			}
		}
		for _, status := range history.UnsupportedStatuses {
			if status.Prompt == id {
				item.BlockedByUnsupported = true
			}
		}
		var current *StatusHistory
		for _, status := range item.Statuses {
			if status.Authoritative && status.Status.State != StateSuperseded {
				item.Dispatched = true
				if status.Status.State.Active() {
					item.Active = true
				}
				if current == nil || laterStatus(status, *current) {
					candidate := status
					current = &candidate
				}
			}
		}
		item.CurrentStatus = current
		item.Pending = !item.Dispatched && !item.BlockedByUnsupported
		if entry.prompt.Continues == nil {
			item.ContinuationValid = true
			item.ContinuationTrusted = true
			item.Eligible = item.Pending
		} else {
			item.ContinuationValid, item.ContinuationTrusted = continuationRelation(*entry.prompt.Continues, history.Statuses)
			item.CanonicalContinuation = canonical[*entry.prompt.Continues] == id
			item.Eligible = item.Pending && item.ContinuationValid && item.ContinuationTrusted && item.CanonicalContinuation
			if !item.ContinuationValid {
				history.Warnings = append(history.Warnings, warning(entry.comment, WarningInvalidContinuation, strconv.FormatInt(int64(*entry.prompt.Continues), 10)))
			} else if !item.CanonicalContinuation {
				history.Warnings = append(history.Warnings, warning(entry.comment, WarningDuplicateContinuation, strconv.FormatInt(int64(*entry.prompt.Continues), 10)))
			}
		}
		history.Prompts = append(history.Prompts, item)
	}
	sortWarnings(history.Warnings)
	return history
}

func laterStatus(left, right StatusHistory) bool {
	if left.Status.State.Active() != right.Status.State.Active() {
		return left.Status.State.Active()
	}
	if !left.Comment.CreatedAt.Equal(right.Comment.CreatedAt) {
		return left.Comment.CreatedAt.After(right.Comment.CreatedAt)
	}
	return left.Comment.ID > right.Comment.ID
}

func continuationRelation(target CommentID, statuses []StatusHistory) (valid, trusted bool) {
	for _, status := range statuses {
		if status.Comment.ID == target && !status.Orphan && status.Status.State == StateInterrupted {
			return true, status.Authoritative
		}
	}
	return false, false
}

func canonicalContinuations(prompts map[CommentID]promptEntry, statuses []StatusHistory) map[CommentID]CommentID {
	result := make(map[CommentID]CommentID)
	for id, entry := range prompts {
		if entry.prompt.Continues == nil || !entry.comment.Trust.Trusted {
			continue
		}
		valid, trusted := continuationRelation(*entry.prompt.Continues, statuses)
		if !valid || !trusted {
			continue
		}
		current, exists := result[*entry.prompt.Continues]
		if !exists || id < current {
			result[*entry.prompt.Continues] = id
		}
	}
	return result
}

func warning(comment Comment, kind WarningKind, detail string) Warning {
	return Warning{CommentID: comment.ID, Kind: kind, Detail: detail}
}

func sortWarnings(warnings []Warning) {
	sort.Slice(warnings, func(i, j int) bool {
		if warnings[i].CommentID != warnings[j].CommentID {
			return warnings[i].CommentID < warnings[j].CommentID
		}
		return warnings[i].Kind < warnings[j].Kind
	})
}
