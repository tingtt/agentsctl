package work

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const protocolVersion = 1

var (
	keyPattern    = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Parse classifies and validates the trailing agentsctl-work metadata block.
// Marker-like text inside fenced code blocks is ordinary discussion text.
func Parse(body string) ParsedComment {
	start, end, count, markerSeen := findProtocolBlock(body)
	if !markerSeen {
		return ParsedComment{Kind: Discussion, VisibleBody: body}
	}
	if count != 1 || start < 0 || end < 0 {
		return malformed(body, errors.New("protocol marker is duplicated or unterminated"))
	}
	if strings.TrimSpace(body[end:]) != "" {
		return malformed(body, errors.New("protocol block is not at the end of the comment"))
	}
	lines, err := splitProtocolBlock(body[start:end])
	if err != nil {
		return malformed(body, err)
	}
	fields := make(map[string]string, len(lines))
	for _, line := range lines {
		key, value, ok := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		if !ok || !keyPattern.MatchString(key) {
			return malformed(body, fmt.Errorf("invalid protocol field %q", line))
		}
		if _, duplicate := fields[key]; duplicate {
			return malformed(body, fmt.Errorf("duplicate protocol field %q", key))
		}
		fields[key] = strings.TrimSpace(value)
	}
	typeName, ok := fields["type"]
	if !ok || typeName == "" {
		return malformed(body, errors.New("missing protocol type"))
	}
	versionText, ok := fields["version"]
	if !ok {
		return malformed(body, errors.New("missing protocol version"))
	}
	version, err := strconv.Atoi(versionText)
	if err != nil || version <= 0 {
		return malformed(body, errors.New("invalid protocol version"))
	}
	result := ParsedComment{VisibleBody: strings.TrimSpace(body[:start]), Type: typeName, Version: version, Fields: fields}
	if typeName != "prompt" && typeName != "status" {
		result.Kind = UnknownProtocol
		return result
	}
	if version != protocolVersion {
		result.Kind = UnsupportedProtocol
		return result
	}
	if typeName == "prompt" {
		prompt, err := parsePrompt(fields)
		if err != nil {
			return malformed(body, err)
		}
		result.Kind, result.Prompt = ValidPrompt, &prompt
		return result
	}
	status, err := parseStatus(fields)
	if err != nil {
		return malformed(body, err)
	}
	result.Kind, result.Status = ValidStatus, &status
	return result
}

func malformed(body string, err error) ParsedComment {
	return ParsedComment{Kind: MalformedProtocol, VisibleBody: body, Err: err}
}

func parsePrompt(fields map[string]string) (Prompt, error) {
	var prompt Prompt
	if text, ok := fields["continues"]; ok {
		id, err := parseCommentID(text)
		if err != nil {
			return Prompt{}, fmt.Errorf("invalid continues: %w", err)
		}
		prompt.Continues = &id
	}
	return prompt, nil
}

func parseStatus(fields map[string]string) (Status, error) {
	required := []string{"prompt", "dispatch", "host", "provider", "session", "state", "prompt-digest"}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return Status{}, fmt.Errorf("missing status field %q", key)
		}
	}
	prompt, err := parseCommentID(fields["prompt"])
	if err != nil {
		return Status{}, fmt.Errorf("invalid prompt: %w", err)
	}
	for _, key := range []string{"dispatch", "host", "provider"} {
		if fields[key] == "" {
			return Status{}, fmt.Errorf("status field %q is empty", key)
		}
	}
	state := State(fields["state"])
	if !validState(state) {
		return Status{}, fmt.Errorf("invalid status state %q", state)
	}
	if !digestPattern.MatchString(fields["prompt-digest"]) {
		return Status{}, errors.New("invalid prompt digest")
	}
	return Status{Prompt: prompt, Dispatch: fields["dispatch"], Host: fields["host"], Provider: fields["provider"], Session: fields["session"], Turn: fields["turn"], State: state, PromptDigest: fields["prompt-digest"]}, nil
}

func parseCommentID(text string) (CommentID, error) {
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("comment ID must be a positive int64")
	}
	return CommentID(id), nil
}

func validState(state State) bool {
	switch state {
	case StateStarting, StateRunning, StateCompleted, StateFailed, StateInterrupted, StateStopped, StateAbandoned, StateSuperseded:
		return true
	default:
		return false
	}
}

func findProtocolBlock(body string) (start, end, count int, seen bool) {
	start = -1
	lines := strings.SplitAfter(body, "\n")
	offset := 0
	inFence := false
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSuffix(strings.TrimSuffix(lines[i], "\n"), "\r")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			offset += len(lines[i])
			continue
		}
		if inFence {
			offset += len(lines[i])
			continue
		}
		canonical := trimmed == "<!--" && i+1 < len(lines) && strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(lines[i+1], "\n"), "\r")) == "agentsctl-work"
		legacy := strings.HasPrefix(trimmed, "<!-- agentsctl-work")
		if canonical || legacy {
			seen = true
			count++
			if start < 0 {
				start = offset + strings.Index(line, "<!--")
				if closeAt := strings.Index(body[start:], "-->"); closeAt >= 0 {
					end = start + closeAt + len("-->")
				}
			}
		}
		offset += len(lines[i])
	}
	return start, end, count, seen
}

func splitProtocolBlock(block string) ([]string, error) {
	block = strings.ReplaceAll(block, "\r\n", "\n")
	canonicalPrefix := "<!--\nagentsctl-work\n"
	legacyPrefix := "<!-- agentsctl-work\n"
	var content string
	switch {
	case strings.HasPrefix(block, canonicalPrefix):
		content = strings.TrimSuffix(strings.TrimPrefix(block, canonicalPrefix), "-->")
	case strings.HasPrefix(block, legacyPrefix):
		content = strings.TrimSuffix(strings.TrimPrefix(block, legacyPrefix), "-->")
	default:
		return nil, errors.New("malformed protocol marker")
	}
	content = strings.TrimSuffix(content, "\n")
	if content == "" {
		return nil, errors.New("empty protocol block")
	}
	return strings.Split(content, "\n"), nil
}

// RenderPrompt appends canonical Prompt metadata to a visible body.
func RenderPrompt(visible string, prompt Prompt) string {
	fields := []string{"type: prompt", "version: 1"}
	if prompt.Continues != nil {
		fields = append(fields, fmt.Sprintf("continues: %d", *prompt.Continues))
	}
	return render(visible, fields)
}

// RenderStatus appends canonical Status metadata to a visible body.
func RenderStatus(visible string, status Status) (string, error) {
	if _, err := parseStatus(statusFields(status)); err != nil {
		return "", err
	}
	fields := []string{
		"type: status", "version: 1", fmt.Sprintf("prompt: %d", status.Prompt),
		"dispatch: " + status.Dispatch, "host: " + status.Host, "provider: " + status.Provider,
		"session: " + status.Session,
	}
	if status.Turn != "" {
		fields = append(fields, "turn: "+status.Turn)
	}
	fields = append(fields, "state: "+string(status.State), "prompt-digest: "+status.PromptDigest)
	return render(visible, fields), nil
}

func statusFields(status Status) map[string]string {
	return map[string]string{"type": "status", "version": "1", "prompt": strconv.FormatInt(int64(status.Prompt), 10), "dispatch": status.Dispatch, "host": status.Host, "provider": status.Provider, "session": status.Session, "turn": status.Turn, "state": string(status.State), "prompt-digest": status.PromptDigest}
}

func render(visible string, fields []string) string {
	visible = strings.TrimSpace(strings.ReplaceAll(visible, "\r\n", "\n"))
	block := "<!--\nagentsctl-work\n" + strings.Join(fields, "\n") + "\n-->"
	if visible == "" {
		return block
	}
	return visible + "\n\n" + block
}
