package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

// controlSocketURL is the handshake URL of the app-server control socket;
// the host is nominal since the transport is a Unix socket.
const controlSocketURL = "ws://localhost/rpc"

// maxRPCMessageBytes bounds one incoming JSON-RPC message (one WebSocket
// text frame). A thread/list page carries thread metadata only, so this is
// generous.
const maxRPCMessageBytes = 16 << 20

// errConnClosed is returned by calls on a connection that has ended.
var errConnClosed = errors.New("codex app-server connection closed")

// rpcConn is one persistent JSON-RPC connection to the app-server control
// socket: one WebSocket text frame carries one JSON-RPC message. Unlike the
// short-lived stdio rpcClient, responses and notifications interleave on a
// long-lived stream, so a single reader goroutine owns every read and routes
// responses to their pending call and notifications to onNotify. Writes are
// serialized. A server-initiated request is never answered: this connection
// only observes, and must not turn an approval or user-input request into a
// decision.
type rpcConn struct {
	ws       *websocket.Conn
	onNotify func(method string, params json.RawMessage)

	// ctx bounds the reader and every write; cancel ends the connection.
	ctx    context.Context
	cancel context.CancelFunc

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcReply
	err     error // why the connection ended; set before done closes

	done chan struct{}
}

type rpcReply struct {
	result json.RawMessage
	err    error
}

// dialRPC opens a connection to the app-server listening on socket.
// onNotify is called from the reader goroutine, in arrival order, for every
// notification; it must not block.
func dialRPC(ctx context.Context, socket string, onNotify func(string, json.RawMessage)) (*rpcConn, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	ws, _, err := websocket.Dial(ctx, controlSocketURL, &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	ws.SetReadLimit(maxRPCMessageBytes)
	connCtx, cancel := context.WithCancel(context.Background())
	c := &rpcConn{ws: ws, onNotify: onNotify, ctx: connCtx, cancel: cancel, pending: map[int64]chan rpcReply{}, done: make(chan struct{})}
	go c.read()
	return c, nil
}

// Done is closed once the connection has ended; Err then says why.
func (c *rpcConn) Done() <-chan struct{} { return c.done }

func (c *rpcConn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection and waits for its reader to stop. Pending calls
// return errConnClosed.
func (c *rpcConn) Close() {
	c.cancel()
	_ = c.ws.CloseNow()
	<-c.done
}

// call sends one request and waits for its response. Cancelling ctx
// abandons the wait (a late response is dropped) without disturbing the
// connection.
func (c *rpcConn) call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return c.err
	}
	c.nextID++
	id := c.nextID
	reply := make(chan rpcReply, 1)
	c.pending[id] = reply
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case r := <-reply:
		if r.err != nil {
			return r.err
		}
		if result != nil {
			return json.Unmarshal(r.result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
}

func (c *rpcConn) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// write sends one message. It runs under the connection's own context, not
// the caller's: coder/websocket closes the connection when a write's
// context ends, and one caller giving up must not end it for everyone.
func (c *rpcConn) write(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.Write(c.ctx, websocket.MessageText, data); err != nil {
		c.fail(err)
		return c.Err()
	}
	return nil
}

// read is the connection's only reader. A malformed or unrecognized message
// is skipped; only a transport failure ends the connection.
func (c *rpcConn) read() {
	for {
		typ, data, err := c.ws.Read(c.ctx)
		if err != nil {
			c.fail(err)
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch {
		case msg.Method != "" && len(msg.ID) > 0:
			// A server request (approval, user input, ...). Deliberately
			// unanswered -- see rpcConn.
		case msg.Method != "":
			c.onNotify(msg.Method, msg.Params)
		case len(msg.ID) > 0:
			var id int64
			if json.Unmarshal(msg.ID, &id) != nil {
				continue
			}
			reply := rpcReply{result: msg.Result}
			if msg.Error != nil {
				reply.err = fmt.Errorf("app-server error %d: %s", msg.Error.Code, msg.Error.Message)
			}
			c.mu.Lock()
			ch, ok := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ok {
				ch <- reply
			}
		}
	}
}

// fail records why the connection ended (the first reason wins) and tears
// it down.
func (c *rpcConn) fail(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	if c.ctx.Err() != nil {
		err = errConnClosed
	}
	c.err = fmt.Errorf("codex app-server connection: %w", err)
	close(c.done)
	c.mu.Unlock()
	c.cancel()
	_ = c.ws.CloseNow()
}

// initializeRPC performs the app-server handshake (initialize, then
// initialized) shared by every connection.
func initializeRPC(ctx context.Context, call func(context.Context, string, any, any) error, notify func(string, any) error, result any) error {
	params := map[string]any{"clientInfo": map[string]string{"name": "agentsctl", "version": "dev"}, "capabilities": map[string]bool{"experimentalApi": false}}
	if err := call(ctx, "initialize", params, result); err != nil {
		return err
	}
	return notify("initialized", struct{}{})
}
