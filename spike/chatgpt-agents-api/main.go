package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: chatgpt-agents-api <list|paginate|diff|items|turns> [flags]")
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return errors.New("BLOCKED: official Agents API credential unavailable (OPENAI_API_KEY not set)")
	}
	c := newClient(apiKey)
	ctx := context.Background()

	switch args[0] {
	case "list":
		return cmdList(ctx, c, args[1:])
	case "paginate":
		return cmdPaginate(ctx, c, args[1:])
	case "diff":
		return cmdDiff(args[1:])
	case "items":
		return cmdItems(ctx, c, args[1:])
	case "turns":
		return cmdTurns(ctx, c, args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

// --- state directory: caches raw session IDs locally (never printed) so items/turns
// can select a session without a raw ID ever appearing as a CLI argument. ---

func stateFiles(dir string) (current, previous, candidate string) {
	return filepath.Join(dir, "last_list.json"),
		filepath.Join(dir, "last_list.prev.json"),
		filepath.Join(dir, "candidate.json")
}

func loadSessionIDs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var resp sessionListResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(resp.Data))
	for _, raw := range resp.Data {
		var s session
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		ids = append(ids, s.ID)
	}
	return ids, nil
}

func cmdList(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	limit := fs.Int("limit", 0, "max sessions to return")
	order := fs.String("order", "", "asc|desc")
	agentID := fs.String("agent-id", "", "filter by agent ID")
	stateDir := fs.String("state-dir", "", "directory to cache the raw response for later diff/select (never committed)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	resp, err := c.listSessions(ctx, listOptions{Limit: *limit, Order: *order, AgentID: *agentID})
	if err != nil {
		return err
	}
	printSessionListDiagnostic(resp)

	if *stateDir != "" {
		if err := os.MkdirAll(*stateDir, 0o700); err != nil {
			return err
		}
		current, previous, _ := stateFiles(*stateDir)
		if data, err := os.ReadFile(current); err == nil {
			if err := os.WriteFile(previous, data, 0o600); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		if err := os.WriteFile(current, raw, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func printSessionListDiagnostic(resp *sessionListResponse) {
	fmt.Println("agents sessions:")
	fmt.Printf("  count_first_page=%d\n", len(resp.sessions))
	fmt.Printf("  has_more=%t\n", resp.HasMore)
	fmt.Printf("  first_id_present=%t\n", resp.FirstID != "")
	fmt.Printf("  last_id_present=%t\n", resp.LastID != "")
	for i, s := range resp.sessions {
		fmt.Printf("  [%d] fingerprint=%s created_at=%d last_active_at=%d status=%s agent_fingerprint=%s metadata_keys=%v extra_keys=%v\n",
			i, fingerprint(s.ID), s.CreatedAt, s.LastActiveAt, s.Status, fingerprint(s.Agent.ID), metadataKeys(s.Metadata), s.ExtraKeys)
	}
}

func metadataKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func cmdPaginate(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("paginate", flag.ContinueOnError)
	limit := fs.Int("limit", 2, "page size")
	order := fs.String("order", "desc", "asc|desc")
	if err := fs.Parse(args); err != nil {
		return err
	}

	page1, err := c.listSessions(ctx, listOptions{Limit: *limit, Order: *order})
	if err != nil {
		return err
	}
	ids1 := sessionIDs(page1)
	fmt.Printf("page1: count=%d has_more=%t\n", len(ids1), page1.HasMore)

	if !page1.HasMore || page1.LastID == "" {
		fmt.Println("page2: skipped (page1 already exhausted)")
		return nil
	}

	page2, err := c.listSessions(ctx, listOptions{Limit: *limit, Order: *order, After: page1.LastID})
	if err != nil {
		return err
	}
	ids2 := sessionIDs(page2)
	fmt.Printf("page2: count=%d has_more=%t\n", len(ids2), page2.HasMore)

	all, hasDup := accumulatePages([][]string{ids1, ids2})
	noOverlap := len(diffSessionIDs(ids1, ids2)) == len(ids2)
	fmt.Printf("cursor_advanced_no_overlap=%t duplicate_across_pages=%t total_accumulated=%d\n", noOverlap, hasDup, len(all))
	return nil
}

func sessionIDs(resp *sessionListResponse) []string {
	ids := make([]string, len(resp.sessions))
	for i, s := range resp.sessions {
		ids[i] = s.ID
	}
	return ids
}

func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "directory previously populated by `list --state-dir`")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *stateDir == "" {
		return errors.New("--state-dir is required")
	}
	current, previous, candidatePath := stateFiles(*stateDir)

	beforeIDs, err := loadSessionIDs(previous)
	if err != nil {
		return fmt.Errorf("load previous state (run `list --state-dir` twice first): %w", err)
	}
	afterIDs, err := loadSessionIDs(current)
	if err != nil {
		return fmt.Errorf("load current state: %w", err)
	}

	newIDs := diffSessionIDs(beforeIDs, afterIDs)
	fmt.Printf("before_count=%d after_count=%d new_count=%d\n", len(beforeIDs), len(afterIDs), len(newIDs))
	for _, id := range newIDs {
		fmt.Printf("  new_session fingerprint=%s\n", fingerprint(id))
	}

	id, ok := controlledCandidate(beforeIDs, afterIDs)
	if !ok {
		fmt.Println("controlled candidate: NOT VERIFIED (need exactly one new session)")
		return nil
	}
	fmt.Printf("controlled candidate: identified (fingerprint=%s)\n", fingerprint(id))
	data, err := json.Marshal(map[string]string{"id": id})
	if err != nil {
		return err
	}
	return os.WriteFile(candidatePath, data, 0o600)
}

func resolveSessionID(stateDir, use string) (string, error) {
	current, _, candidatePath := stateFiles(stateDir)
	switch {
	case use == "candidate":
		data, err := os.ReadFile(candidatePath)
		if err != nil {
			return "", fmt.Errorf("no candidate recorded (run `diff` first): %w", err)
		}
		var v struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(data, &v); err != nil {
			return "", err
		}
		return v.ID, nil
	case use == "newest":
		ids, err := loadSessionIDs(current)
		if err != nil {
			return "", err
		}
		if len(ids) == 0 {
			return "", errors.New("no sessions in cached state (run `list --state-dir` first)")
		}
		return ids[0], nil
	default:
		var idx int
		if _, err := fmt.Sscanf(use, "index:%d", &idx); err != nil {
			return "", fmt.Errorf("--use must be candidate, newest, or index:N: %w", err)
		}
		ids, err := loadSessionIDs(current)
		if err != nil {
			return "", err
		}
		if idx < 0 || idx >= len(ids) {
			return "", fmt.Errorf("index %d out of range (0..%d)", idx, len(ids)-1)
		}
		return ids[idx], nil
	}
}

func cmdItems(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("items", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "directory populated by `list --state-dir`")
	use := fs.String("use", "candidate", "candidate|newest|index:N")
	marker := fs.String("marker", "", "controlled marker text to search for in item content")
	limit := fs.Int("limit", 100, "max items to return")
	order := fs.String("order", "asc", "asc|desc")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *stateDir == "" {
		return errors.New("--state-dir is required")
	}
	id, err := resolveSessionID(*stateDir, *use)
	if err != nil {
		return err
	}
	resp, err := c.listItems(ctx, id, listOptions{Limit: *limit, Order: *order})
	if err != nil {
		return err
	}
	fmt.Printf("session items: fingerprint=%s count=%d has_more=%t\n", fingerprint(id), len(resp.Data), resp.HasMore)
	if *marker != "" {
		fmt.Printf("marker_found=%t\n", markerFound(resp.Data, *marker))
	}
	return nil
}

func cmdTurns(ctx context.Context, c *client, args []string) error {
	fs := flag.NewFlagSet("turns", flag.ContinueOnError)
	stateDir := fs.String("state-dir", "", "directory populated by `list --state-dir`")
	use := fs.String("use", "candidate", "candidate|newest|index:N")
	limit := fs.Int("limit", 100, "max turns to return")
	order := fs.String("order", "asc", "asc|desc")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *stateDir == "" {
		return errors.New("--state-dir is required")
	}
	id, err := resolveSessionID(*stateDir, *use)
	if err != nil {
		return err
	}
	resp, err := c.listTurns(ctx, id, listOptions{Limit: *limit, Order: *order})
	if err != nil {
		return err
	}
	fmt.Printf("session turns: fingerprint=%s count=%d has_more=%t\n", fingerprint(id), len(resp.Data), resp.HasMore)
	for i, t := range resp.Data {
		fmt.Printf("  [%d] turn_fingerprint=%s status=%s created_at=%d started_at=%d completed_at=%d has_error=%t\n",
			i, fingerprint(t.ID), t.Status, t.CreatedAt, t.StartedAt, t.CompletedAt, len(t.Error) > 0 && string(t.Error) != "null")
	}
	return nil
}
