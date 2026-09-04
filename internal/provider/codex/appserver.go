package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

type ThreadStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags"`
}
type Turn struct {
	Status string `json:"status"`
}
type Thread struct {
	ID        string       `json:"id"`
	Name      *string      `json:"name"`
	Preview   *string      `json:"preview"`
	CWD       string       `json:"cwd"`
	CreatedAt int64        `json:"createdAt"`
	UpdatedAt int64        `json:"updatedAt"`
	Status    ThreadStatus `json:"status"`
	Turns     []Turn       `json:"turns"`
}

type AppServer interface {
	List(context.Context, bool) ([]Thread, error)
	Rename(context.Context, string, string) error
	Archive(context.Context, string) error
	Unarchive(context.Context, string) error
	CodexHome() string
}

type CommandAppServer struct {
	Path string
	mu   sync.RWMutex
	home string
}

func (c *CommandAppServer) CodexHome() string { c.mu.RLock(); defer c.mu.RUnlock(); return c.home }

func (c *CommandAppServer) withClient(ctx context.Context, fn func(*rpcClient) error) error {
	path := c.Path
	if path == "" {
		path = "codex"
	}
	cmd := exec.CommandContext(ctx, path, "app-server", "--stdio")
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start codex app-server: %w", err)
	}
	cl := &rpcClient{enc: json.NewEncoder(in), scan: bufio.NewScanner(out), in: in}
	cl.scan.Buffer(make([]byte, 64<<10), 8<<20)
	defer func() { _ = in.Close(); _ = cmd.Wait() }()
	var init struct {
		CodexHome string `json:"codexHome"`
	}
	if err := cl.call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": "agentsctl", "version": "dev"}, "capabilities": map[string]bool{"experimentalApi": false}}, &init); err != nil {
		return err
	}
	if err := cl.notify("initialized", struct{}{}); err != nil {
		return err
	}
	c.mu.Lock()
	c.home = init.CodexHome
	c.mu.Unlock()
	return fn(cl)
}

func (c *CommandAppServer) List(ctx context.Context, archived bool) ([]Thread, error) {
	var order []string
	byID := map[string]Thread{}
	err := c.withClient(ctx, func(cl *rpcClient) error {
		var cursor *string
		seen := map[string]bool{}
		for {
			var res struct {
				Data []Thread `json:"data"`
				Next *string  `json:"nextCursor"`
			}
			params := map[string]any{"limit": 100, "archived": archived}
			if cursor != nil {
				params["cursor"] = *cursor
			}
			if err := cl.call(ctx, "thread/list", params, &res); err != nil {
				return err
			}
			for _, t := range res.Data {
				mergeThread(byID, &order, t)
			}
			if res.Next == nil || *res.Next == "" {
				return nil
			}
			if seen[*res.Next] {
				return errors.New("thread/list repeated cursor")
			}
			seen[*res.Next] = true
			cursor = res.Next
		}
	})
	if err != nil {
		return nil, err
	}
	rows := make([]Thread, 0, len(order))
	for _, id := range order {
		rows = append(rows, byID[id])
	}
	return rows, nil
}

// mergeThread keeps at most one Thread per ID. The installed codex CLI's
// app-server has been observed (via its real thread/list response) to
// report the same thread ID twice within a single page, with different
// UpdatedAt/path values, when a session was resumed into a new rollout
// file under the same thread ID. Duplicates are also merged across pages
// since a thread can in principle straddle a page boundary. The row with
// the newest UpdatedAt wins, so the result is deterministic regardless of
// which copy the app-server happened to list first.
func mergeThread(byID map[string]Thread, order *[]string, t Thread) {
	existing, ok := byID[t.ID]
	if !ok {
		byID[t.ID] = t
		*order = append(*order, t.ID)
		return
	}
	if t.UpdatedAt > existing.UpdatedAt {
		byID[t.ID] = t
	}
}
func (c *CommandAppServer) Rename(ctx context.Context, id, name string) error {
	return c.withClient(ctx, func(cl *rpcClient) error {
		return cl.call(ctx, "thread/name/set", map[string]string{"threadId": id, "name": name}, nil)
	})
}
func (c *CommandAppServer) Archive(ctx context.Context, id string) error {
	return c.withClient(ctx, func(cl *rpcClient) error {
		return cl.call(ctx, "thread/archive", map[string]string{"threadId": id}, nil)
	})
}
func (c *CommandAppServer) Unarchive(ctx context.Context, id string) error {
	return c.withClient(ctx, func(cl *rpcClient) error {
		return cl.call(ctx, "thread/unarchive", map[string]string{"threadId": id}, nil)
	})
}

type rpcClient struct {
	enc  *json.Encoder
	scan *bufio.Scanner
	in   io.WriteCloser
	id   int
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (r *rpcClient) notify(method string, params any) error {
	return r.enc.Encode(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func (r *rpcClient) call(ctx context.Context, method string, params, result any) error {
	r.id++
	id := r.id
	if err := r.enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	for r.scan.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal(r.scan.Bytes(), &msg) != nil || len(msg.ID) == 0 {
			continue
		}
		var got int
		if json.Unmarshal(msg.ID, &got) != nil || got != id {
			continue
		}
		if msg.Error != nil {
			return fmt.Errorf("app-server error %d: %s", msg.Error.Code, msg.Error.Message)
		}
		if result != nil {
			return json.Unmarshal(msg.Result, result)
		}
		return nil
	}
	if err := r.scan.Err(); err != nil {
		return err
	}
	return io.EOF
}
