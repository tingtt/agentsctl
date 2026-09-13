// Command chatgpt-agents-api probes the official OpenAI Agents API to test whether a
// ChatGPT Work session is the same object as a Managed Agents session, for issue #7.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	apiBase    = "https://api.openai.com/v1"
	betaHeader = "agents=v1"
)

// client is a minimal, read-only Agents API client. It never issues POST/DELETE/update
// calls: only the List/Get read operations this spike is permitted to use.
type client struct {
	apiKey string
	http   *http.Client
}

func newClient(apiKey string) *client {
	return &client{apiKey: apiKey, http: &http.Client{Timeout: 30 * time.Second}}
}

func (c *client) get(ctx context.Context, path string, query url.Values, out any) error {
	u := apiBase + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("OpenAI-Beta", betaHeader)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &apiErr)
		return fmt.Errorf("agents API %s %s: HTTP %d: %s: %s", http.MethodGet, path, resp.StatusCode, apiErr.Error.Type, apiErr.Error.Message)
	}
	return json.Unmarshal(body, out)
}

// session is the subset of the documented AgentSession fields this spike needs.
// ExtraKeys captures the names (never values) of any top-level field this struct does
// not already model, so an undocumented ChatGPT/Project-identity field would surface as
// a new key name without this spike ever printing its value.
type session struct {
	ID              string            `json:"id"`
	Agent           sessionAgent      `json:"agent"`
	CreatedAt       int64             `json:"created_at"`
	LastActiveAt    int64             `json:"last_active_at"`
	Status          string            `json:"status"`
	RequiredActions []json.RawMessage `json:"required_actions"`
	Metadata        map[string]string `json:"metadata"`
	Environment     json.RawMessage   `json:"environment"`
	VaultIDs        []string          `json:"vault_ids"`
	Usage           json.RawMessage   `json:"usage"`
	Error           string            `json:"error"`

	ExtraKeys []string `json:"-"`
}

type sessionAgent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *session) computeExtraKeys(raw json.RawMessage) {
	known := map[string]bool{
		"id": true, "agent": true, "created_at": true, "last_active_at": true,
		"status": true, "required_actions": true, "metadata": true,
		"environment": true, "vault_ids": true, "usage": true, "error": true,
		"object": true,
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	for k := range m {
		if !known[k] {
			s.ExtraKeys = append(s.ExtraKeys, k)
		}
	}
}

type sessionListResponse struct {
	Object  string            `json:"object"`
	Data    []json.RawMessage `json:"data"`
	FirstID string            `json:"first_id"`
	LastID  string            `json:"last_id"`
	HasMore bool              `json:"has_more"`

	sessions []session
}

func (c *client) listSessions(ctx context.Context, opt listOptions) (*sessionListResponse, error) {
	q := url.Values{}
	if opt.After != "" {
		q.Set("after", opt.After)
	}
	if opt.AgentID != "" {
		q.Set("agent_id", opt.AgentID)
	}
	if opt.Limit > 0 {
		q.Set("limit", strconv.Itoa(opt.Limit))
	}
	if opt.Order != "" {
		q.Set("order", opt.Order)
	}
	var resp sessionListResponse
	if err := c.get(ctx, "/agents/sessions", q, &resp); err != nil {
		return nil, err
	}
	resp.sessions = make([]session, len(resp.Data))
	for i, raw := range resp.Data {
		if err := json.Unmarshal(raw, &resp.sessions[i]); err != nil {
			return nil, fmt.Errorf("decode session %d: %w", i, err)
		}
		resp.sessions[i].computeExtraKeys(raw)
	}
	return &resp, nil
}

type listOptions struct {
	After   string
	AgentID string
	Limit   int
	Order   string
}

type turn struct {
	ID          string          `json:"id"`
	AgentID     string          `json:"agent_id"`
	SessionID   string          `json:"session_id"`
	SubagentID  string          `json:"subagent_id"`
	Status      string          `json:"status"`
	CreatedAt   int64           `json:"created_at"`
	StartedAt   int64           `json:"started_at"`
	CompletedAt int64           `json:"completed_at"`
	Error       json.RawMessage `json:"error"`
	Usage       json.RawMessage `json:"usage"`
}

type turnListResponse struct {
	Object  string `json:"object"`
	Data    []turn `json:"data"`
	FirstID string `json:"first_id"`
	LastID  string `json:"last_id"`
	HasMore bool   `json:"has_more"`
}

func (c *client) listTurns(ctx context.Context, sessionID string, opt listOptions) (*turnListResponse, error) {
	q := url.Values{}
	if opt.After != "" {
		q.Set("after", opt.After)
	}
	if opt.Limit > 0 {
		q.Set("limit", strconv.Itoa(opt.Limit))
	}
	if opt.Order != "" {
		q.Set("order", opt.Order)
	}
	var resp turnListResponse
	if err := c.get(ctx, "/agents/sessions/"+url.PathEscape(sessionID)+"/turns", q, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

type itemListResponse struct {
	Object  string            `json:"object"`
	Data    []json.RawMessage `json:"data"`
	FirstID string            `json:"first_id"`
	LastID  string            `json:"last_id"`
	HasMore bool              `json:"has_more"`
}

func (c *client) listItems(ctx context.Context, sessionID string, opt listOptions) (*itemListResponse, error) {
	q := url.Values{}
	if opt.After != "" {
		q.Set("after", opt.After)
	}
	if opt.Limit > 0 {
		q.Set("limit", strconv.Itoa(opt.Limit))
	}
	if opt.Order != "" {
		q.Set("order", opt.Order)
	}
	var resp itemListResponse
	if err := c.get(ctx, "/agents/sessions/"+url.PathEscape(sessionID)+"/items", q, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
