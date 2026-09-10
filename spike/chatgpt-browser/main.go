// Command chatgpt-browser proves the process and security boundaries proposed by issue #7.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

const protocolVersion = 1

type config struct {
	binary        string
	partition     string
	url           string
	socket        string
	projectID     string
	projectName   string
	hold          time.Duration
	closePTYAfter time.Duration
}

type request struct {
	ID              int      `json:"id"`
	Method          string   `json:"method"`
	ProjectID       string   `json:"projectID,omitempty"`
	ConversationIDs []string `json:"conversationIDs,omitempty"`
}

type response struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type pingInfo struct {
	AttachedDebuggerCount           int  `json:"attachedDebuggerCount"`
	MatchedResponseCount            int  `json:"matchedResponseCount"`
	CaptureErrorCount               int  `json:"captureErrorCount"`
	ProjectsCaptured                bool `json:"projectsCaptured"`
	ConversationCollectionsCaptured int  `json:"conversationCollectionsCaptured"`
}

type pageInfo struct {
	Href                  string   `json:"href"`
	Title                 string   `json:"title"`
	ReadyState            string   `json:"readyState"`
	LoginPromptVisible    bool     `json:"loginPromptVisible"`
	AccountControlPresent bool     `json:"accountControlPresent"`
	ConversationLinkCount int      `json:"conversationLinkCount"`
	ProjectLinkCount      int      `json:"projectLinkCount"`
	ObservedBackendPaths  []string `json:"observedBackendPaths"`
}

type project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type conversation struct {
	ID            string         `json:"id"`
	Title         string         `json:"title"`
	ProjectID     string         `json:"projectID"`
	Discriminator map[string]any `json:"discriminator"`
	AvailableKeys []string       `json:"availableKeys"`
}

type conversationEvidence struct {
	Path               string   `json:"path"`
	PresentCount       int      `json:"presentCount"`
	DistinctValueCount int      `json:"distinctValueCount"`
	Values             []string `json:"values"`
	Types              []string `json:"types"`
}

type urlProbeResult struct {
	Style                   string `json:"style"`
	ReadyState              string `json:"readyState"`
	LoginPromptVisible      bool   `json:"loginPromptVisible"`
	HrefMatchesExpectedPath bool   `json:"hrefMatchesExpectedPath"`
	RedactedHref            string `json:"redactedHref"`
	RedactedExpectedPath    string `json:"redactedExpectedPath"`
}

type openURLProbeResult struct {
	WorkLikeCanonical     urlProbeResult `json:"workLikeCanonical"`
	WorkLikeProjectScoped urlProbeResult `json:"workLikeProjectScoped"`
	ChatLikeCanonical     urlProbeResult `json:"chatLikeCanonical"`
	ChatLikeProjectScoped urlProbeResult `json:"chatLikeProjectScoped"`
}

type taskEvidence struct {
	ConversationIDMatches         int      `json:"conversationIDMatches"`
	OverlapWithKnownConversations int      `json:"overlapWithKnownConversations"`
	StatusFieldCandidates         []string `json:"statusFieldCandidates"`
	DistinctStatusValueCount      int      `json:"distinctStatusValueCount"`
}

func main() {
	if err := run(context.Background(), parseFlags()); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var cfg config
	flag.StringVar(&cfg.binary, "terminal-browser", "terminal-browser", "terminal-browser executable")
	flag.StringVar(&cfg.partition, "partition", "agentsctl-chatgpt", "persistent browser partition")
	flag.StringVar(&cfg.url, "url", "https://chatgpt.com", "initial ChatGPT URL")
	flag.StringVar(&cfg.socket, "socket", filepath.Join(os.TempDir(), "agentsctl-chatgpt-browser.sock"), "bridge socket")
	flag.StringVar(&cfg.projectID, "project-id", "", "Project ID to query")
	flag.StringVar(&cfg.projectName, "project-name", "", "Project name to resolve without printing names")
	flag.DurationVar(&cfg.hold, "hold", 0, "keep the helper alive after probes")
	flag.DurationVar(&cfg.closePTYAfter, "close-pty-after", 0, "close the PTY after this duration for lifecycle testing")
	flag.Parse()
	return cfg
}

func run(ctx context.Context, cfg config) error {
	if runtime.GOOS == "windows" {
		return errors.New("the pseudo-PTY spike requires a Unix host")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	mainScript := filepath.Join(root, "bridge", "main.js")
	preload := filepath.Join(root, "bridge", "preload.js")
	for _, path := range []string{mainScript, preload} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("bridge asset %s: %w", path, err)
		}
	}

	cmd := exec.CommandContext(ctx, cfg.binary,
		"open", cfg.url,
		"--no-merge",
		"--partition="+cfg.partition,
		"--main-script="+mainScript,
		"--preload="+preload,
	)
	cmd.Env = append(os.Environ(),
		"TERMINAL_BROWSER_SKIP_GRAPHICS_CHECK=1",
		"AGENTSCTL_CHATGPT_BRIDGE_SOCKET="+cfg.socket,
	)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("start terminal-browser: %w", err)
	}
	defer ptmx.Close()
	go func() { _, _ = io.Copy(io.Discard, ptmx) }()

	if cfg.closePTYAfter > 0 {
		go func() {
			time.Sleep(cfg.closePTYAfter)
			_ = ptmx.Close()
		}()
	}

	client, err := dialBridge(ctx, cfg.socket, 20*time.Second)
	if err != nil {
		_ = terminate(cmd)
		return err
	}
	defer client.Close()

	if _, err := call(client, request{ID: 1, Method: "ping"}); err != nil {
		return err
	}
	fmt.Printf("bridge round trip: PASS (protocol %d)\n", protocolVersion)

	raw, err := call(client, request{ID: 2, Method: "pageInfo"})
	if err != nil {
		return err
	}
	var page pageInfo
	if err := json.Unmarshal(raw, &page); err != nil {
		return fmt.Errorf("decode pageInfo: %w", err)
	}
	fmt.Printf("page: origin=%s ready=%s login_prompt=%t account_control=%t conversation_links=%d project_links=%d title_present=%t\n",
		origin(page.Href), page.ReadyState, page.LoginPromptVisible, page.AccountControlPresent,
		page.ConversationLinkCount, page.ProjectLinkCount, page.Title != "")
	if err := client.Close(); err != nil {
		return fmt.Errorf("close first bridge connection: %w", err)
	}
	client, err = dialBridge(ctx, cfg.socket, 5*time.Second)
	if err != nil {
		return fmt.Errorf("reconnect bridge: %w", err)
	}
	defer client.Close()
	if _, err := call(client, request{ID: 8, Method: "ping"}); err != nil {
		return fmt.Errorf("bridge reconnect round trip: %w", err)
	}
	fmt.Println("bridge client reconnect: PASS")

	projectID := cfg.projectID
	if cfg.projectName != "" || projectID != "" {
		time.Sleep(3 * time.Second)
		raw, err = call(client, request{ID: 9, Method: "pageInfo"})
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("decode settled pageInfo: %w", err)
		}
		fmt.Printf("settled page: ready=%s login_prompt=%t account_control=%t conversation_links=%d project_links=%d\n",
			page.ReadyState, page.LoginPromptVisible, page.AccountControlPresent,
			page.ConversationLinkCount, page.ProjectLinkCount)
		if len(page.ObservedBackendPaths) > 0 {
			fmt.Printf("observed redacted backend paths: %s\n", strings.Join(page.ObservedBackendPaths, ", "))
		}
		raw, err = call(client, request{ID: 10, Method: "ping"})
		if err != nil {
			return err
		}
		var capture pingInfo
		if err := json.Unmarshal(raw, &capture); err != nil {
			return fmt.Errorf("decode capture status: %w", err)
		}
		fmt.Printf("capture: debuggers=%d matched_responses=%d errors=%d projects=%t conversation_collections=%d\n",
			capture.AttachedDebuggerCount, capture.MatchedResponseCount, capture.CaptureErrorCount,
			capture.ProjectsCaptured, capture.ConversationCollectionsCaptured)
		raw, err = call(client, request{ID: 3, Method: "projects"})
		if err != nil {
			return fmt.Errorf("discover projects: %w", err)
		}
		var projects []project
		if err := json.Unmarshal(raw, &projects); err != nil {
			return fmt.Errorf("decode projects: %w", err)
		}
		fmt.Printf("projects: PASS count=%d (names and IDs not logged)\n", len(projects))
		if projectID == "" {
			for _, candidate := range projects {
				if candidate.Name == cfg.projectName {
					if projectID != "" {
						return fmt.Errorf("Project name %q is ambiguous", cfg.projectName)
					}
					projectID = candidate.ID
				}
			}
			if projectID == "" {
				return fmt.Errorf("Project name %q was not found", cfg.projectName)
			}
		}
	}

	if projectID != "" {
		first, err := discoverConversations(client, 4, projectID)
		if err != nil {
			return err
		}
		second, err := discoverConversations(client, 5, projectID)
		if err != nil {
			return err
		}
		if !sameIDs(first, second) {
			return errors.New("conversation IDs changed between repeated discovery calls")
		}
		fmt.Printf("conversations: PASS count=%d repeated_ids_stable=true (titles and IDs not logged)\n", len(first))
		printDiscriminatorEvidence(first)
		ids := make([]string, 0, len(first))
		for _, item := range first {
			ids = append(ids, item.ID)
		}
		raw, err = call(client, request{ID: 11, Method: "conversationEvidence", ConversationIDs: ids})
		if err != nil {
			return fmt.Errorf("compare conversation evidence: %w", err)
		}
		var evidence []conversationEvidence
		if err := json.Unmarshal(raw, &evidence); err != nil {
			return fmt.Errorf("decode conversation evidence: %w", err)
		}
		fmt.Printf("conversation detail evidence: fields=%d sessions=%d (values are structural marker labels only; conversation content, titles, and per-conversation identity mapping are never logged)\n",
			len(evidence), len(first))
		for _, field := range evidence {
			fmt.Printf("evidence field: path=%s present=%d distinct_values=%d values=%s types=%s\n",
				field.Path, field.PresentCount, field.DistinctValueCount, strings.Join(field.Values, "|"), strings.Join(field.Types, ","))
		}

		raw, err = call(client, request{ID: 12, Method: "tasks", ConversationIDs: ids})
		if err != nil {
			fmt.Printf("tasks: NOT VERIFIED (%v)\n", err)
		} else {
			var tasks taskEvidence
			if err := json.Unmarshal(raw, &tasks); err != nil {
				return fmt.Errorf("decode tasks: %w", err)
			}
			fmt.Printf("tasks: PASS conversation_id_like=%d overlap_with_project_conversations=%d status_fields=%s distinct_status_values=%d\n",
				tasks.ConversationIDMatches, tasks.OverlapWithKnownConversations,
				strings.Join(tasks.StatusFieldCandidates, ","), tasks.DistinctStatusValueCount)
		}

		raw, err = call(client, request{ID: 13, Method: "globalConversations", ProjectID: projectID})
		if err != nil {
			fmt.Printf("global conversations: NOT VERIFIED (%v)\n", err)
		} else {
			var global []conversation
			if err := json.Unmarshal(raw, &global); err != nil {
				return fmt.Errorf("decode global conversations: %w", err)
			}
			known := make(map[string]struct{}, len(first))
			for _, item := range first {
				known[item.ID] = struct{}{}
			}
			var newItems []conversation
			for _, item := range global {
				if _, ok := known[item.ID]; !ok {
					newItems = append(newItems, item)
				}
			}
			fmt.Printf("global conversations: PASS project_scoped_via_global=%d new_beyond_project_list=%d\n", len(global), len(newItems))
			if len(newItems) > 0 {
				combinedIDs := make([]string, 0, len(ids)+len(newItems))
				combinedIDs = append(combinedIDs, ids...)
				for _, item := range newItems {
					if len(combinedIDs) >= 20 {
						break
					}
					combinedIDs = append(combinedIDs, item.ID)
				}
				raw, err = call(client, request{ID: 14, Method: "conversationEvidence", ConversationIDs: combinedIDs})
				if err != nil {
					fmt.Printf("combined evidence (known + newly discovered): NOT VERIFIED (%v)\n", err)
				} else {
					var combinedEvidence []conversationEvidence
					if err := json.Unmarshal(raw, &combinedEvidence); err != nil {
						return fmt.Errorf("decode combined evidence: %w", err)
					}
					fmt.Printf("combined evidence: fields=%d sessions=%d (compare distinct_values/values here against the project-only evidence above; a field whose distinct_values rises only now is a discriminator candidate)\n",
						len(combinedEvidence), len(combinedIDs))
					for _, field := range combinedEvidence {
						fmt.Printf("combined evidence field: path=%s present=%d distinct_values=%d values=%s types=%s\n",
							field.Path, field.PresentCount, field.DistinctValueCount, strings.Join(field.Values, "|"), strings.Join(field.Types, ","))
					}
				}
			}
		}

		raw, err = call(client, request{ID: 15, Method: "openURLProbe", ProjectID: projectID})
		if err != nil {
			fmt.Printf("open URL probe: NOT VERIFIED (%v)\n", err)
		} else {
			var probe openURLProbeResult
			if err := json.Unmarshal(raw, &probe); err != nil {
				return fmt.Errorf("decode open URL probe: %w", err)
			}
			printProbe := func(label string, r urlProbeResult) {
				fmt.Printf("open URL probe (%s): style=%s ready=%s login_prompt=%t href_matches_expected_path=%t redacted_href=%s redacted_expected=%s\n",
					label, r.Style, r.ReadyState, r.LoginPromptVisible, r.HrefMatchesExpectedPath, r.RedactedHref, r.RedactedExpectedPath)
			}
			printProbe("work-like, canonical /c/{id}", probe.WorkLikeCanonical)
			printProbe("work-like, project-scoped /g/{project}/c/{id}", probe.WorkLikeProjectScoped)
			printProbe("chat-like, canonical /c/{id}", probe.ChatLikeCanonical)
			printProbe("chat-like, project-scoped /g/{project}/c/{id}", probe.ChatLikeProjectScoped)
		}
	}

	if cfg.hold > 0 {
		fmt.Printf("holding helper for %s\n", cfg.hold)
		timer := time.NewTimer(cfg.hold)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if _, err := call(client, request{ID: 6, Method: "ping"}); err != nil {
			return fmt.Errorf("helper did not survive hold: %w", err)
		}
		raw, err := call(client, request{ID: 7, Method: "pageInfo"})
		if err != nil {
			return fmt.Errorf("page unavailable after hold: %w", err)
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("decode pageInfo after hold: %w", err)
		}
		fmt.Printf("helper sustained: PASS ready=%s origin=%s\n", page.ReadyState, origin(page.Href))
	}
	if err := terminate(cmd); err != nil {
		return err
	}
	return nil
}

func dialBridge(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", path)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("bridge unavailable after %s: %w", timeout, lastErr)
}

func call(conn net.Conn, req request) (json.RawMessage, error) {
	if err := conn.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		return nil, err
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("send %s: %w", req.Method, err)
	}
	var res response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&res); err != nil {
		return nil, fmt.Errorf("receive %s: %w", req.Method, err)
	}
	if res.ID != req.ID {
		return nil, fmt.Errorf("response ID mismatch: got %d, want %d", res.ID, req.ID)
	}
	if !res.OK {
		return nil, errors.New(res.Error)
	}
	if len(res.Result) == 0 {
		return nil, errors.New("successful response has no result")
	}
	return res.Result, nil
}

func discoverConversations(conn net.Conn, id int, projectID string) ([]conversation, error) {
	raw, err := call(conn, request{ID: id, Method: "conversations", ProjectID: projectID})
	if err != nil {
		return nil, fmt.Errorf("discover conversations: %w", err)
	}
	var conversations []conversation
	if err := json.Unmarshal(raw, &conversations); err != nil {
		return nil, fmt.Errorf("decode conversations: %w", err)
	}
	for _, item := range conversations {
		if item.ID == "" || item.ProjectID != projectID {
			return nil, errors.New("conversation response is missing stable identity or Project association")
		}
	}
	return conversations, nil
}

func sameIDs(left, right []conversation) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]struct{}, len(left))
	for _, item := range left {
		seen[item.ID] = struct{}{}
	}
	for _, item := range right {
		if _, ok := seen[item.ID]; !ok {
			return false
		}
	}
	return true
}

func printDiscriminatorEvidence(conversations []conversation) {
	keys := make(map[string]struct{})
	values := make(map[string]struct{})
	relevantSchemaKeys := make(map[string]struct{})
	for _, item := range conversations {
		for key, value := range item.Discriminator {
			keys[key] = struct{}{}
			values[fmt.Sprintf("%s=%v", key, value)] = struct{}{}
		}
		for _, key := range item.AvailableKeys {
			lower := strings.ToLower(key)
			for _, marker := range []string{"work", "agent", "async", "type", "kind", "status", "mode"} {
				if strings.Contains(lower, marker) {
					relevantSchemaKeys[key] = struct{}{}
					break
				}
			}
		}
	}
	fmt.Printf("discriminator candidates: fields=%s distinct_values=%d relevant_schema_fields=%s\n",
		strings.Join(sortedSet(keys), ","), len(values), strings.Join(sortedSet(relevantSchemaKeys), ","))
}

func sortedSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

func origin(rawURL string) string {
	if strings.HasPrefix(rawURL, "https://chatgpt.com") {
		return "https://chatgpt.com"
	}
	return "unexpected"
}

func terminate(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Signal(syscall.SIGTERM)
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stop terminal-browser: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	select {
	case <-time.After(5 * time.Second):
		return errors.New("terminal-browser did not stop after SIGTERM")
	case err := <-wait:
		if err == nil || strings.Contains(err.Error(), "signal: terminated") {
			return nil
		}
		return fmt.Errorf("terminal-browser exited: %w", err)
	}
}
