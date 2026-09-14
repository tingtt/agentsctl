package chatgpt

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

type scrollRegion struct {
	Found                 bool `json:"found"`
	ProjectLinkCount      int  `json:"projectLinkCount"`
	ConversationLinkCount int  `json:"conversationLinkCount"`
	ScrollTop             int  `json:"scrollTop"`
	ScrollHeight          int  `json:"scrollHeight"`
	ClientHeight          int  `json:"clientHeight"`
}

type wheelResult struct {
	Found   bool         `json:"found"`
	Initial scrollRegion `json:"initial"`
	Final   scrollRegion `json:"final"`
	Ticks   int          `json:"ticks"`
}

type discoveryBridge interface {
	BeginList(context.Context, string) error
	Captures(context.Context, string) ([]capture, error)
	ScrollRegion(context.Context, string) (scrollRegion, error)
	Wheel(context.Context, string, int) (wheelResult, error)
	Close() error
}

type bridgeClient struct {
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
	nextID int
}

type bridgeRequest struct {
	ID        int    `json:"id"`
	Method    string `json:"method"`
	ProjectID string `json:"projectID,omitempty"`
	Ticks     int    `json:"ticks,omitempty"`
}

type bridgeResponse struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func newBridgeClient(conn net.Conn) *bridgeClient {
	return &bridgeClient{conn: conn, reader: bufio.NewReader(conn), nextID: 1}
}

func (b *bridgeClient) Ping(ctx context.Context) error {
	var result struct {
		Protocol int `json:"protocol"`
	}
	if err := b.call(ctx, bridgeRequest{Method: "ping"}, &result); err != nil {
		return err
	}
	if result.Protocol != 1 {
		return fmt.Errorf("unsupported browser bridge protocol %d", result.Protocol)
	}
	return nil
}

func (b *bridgeClient) BeginList(ctx context.Context, projectID string) error {
	return b.call(ctx, bridgeRequest{Method: "beginList", ProjectID: projectID}, &struct{}{})
}

func (b *bridgeClient) Captures(ctx context.Context, projectID string) ([]capture, error) {
	var result []capture
	if err := b.call(ctx, bridgeRequest{Method: "captures", ProjectID: projectID}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (b *bridgeClient) ScrollRegion(ctx context.Context, projectID string) (scrollRegion, error) {
	var result scrollRegion
	err := b.call(ctx, bridgeRequest{Method: "scrollRegion", ProjectID: projectID}, &result)
	return result, err
}

func (b *bridgeClient) Wheel(ctx context.Context, projectID string, ticks int) (wheelResult, error) {
	var result wheelResult
	err := b.call(ctx, bridgeRequest{Method: "wheel", ProjectID: projectID, Ticks: ticks}, &result)
	return result, err
}

func (b *bridgeClient) Close() error { return b.conn.Close() }

func (b *bridgeClient) call(ctx context.Context, request bridgeRequest, target any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	request.ID = b.nextID
	deadline := time.Now().Add(20 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := b.conn.SetDeadline(deadline); err != nil {
		return err
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = b.conn.SetDeadline(time.Now()) })
	defer stopCancellation()
	if err := json.NewEncoder(b.conn).Encode(request); err != nil {
		return fmt.Errorf("send browser bridge %s: %w", request.Method, err)
	}
	var response bridgeResponse
	if err := json.NewDecoder(b.reader).Decode(&response); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ctx.Err()
		}
		return fmt.Errorf("receive browser bridge %s: %w", request.Method, err)
	}
	if response.ID != request.ID {
		return fmt.Errorf("browser bridge response ID mismatch")
	}
	if !response.OK {
		return fmt.Errorf("browser bridge %s: %s", request.Method, response.Error)
	}
	if len(response.Result) == 0 {
		return fmt.Errorf("browser bridge %s returned no result", request.Method)
	}
	if err := json.Unmarshal(response.Result, target); err != nil {
		return fmt.Errorf("decode browser bridge %s: %w", request.Method, err)
	}
	return nil
}
