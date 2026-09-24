package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tingtt/agentsctl/internal/localstate"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// fakeDaemon is a minimal scripted Codex app-server listening on a Unix
// socket with the WebSocket transport: initialize, thread/list,
// thread/loaded/list, thread/read, server-sent notifications and forced
// disconnects. It implements only what the runtime uses.
type fakeDaemon struct {
	t      *testing.T
	socket string
	ln     net.Listener
	srv    *http.Server

	mu       sync.Mutex
	threads  []Thread                // thread/list
	loaded   map[string]ThreadStatus // thread/loaded/list + thread/read
	pageSize int                     // thread/list page size; 0 = one page
	// beforeRead, when set, runs before a thread/read is answered (on the
	// connection's handler goroutine, so a notification it sends precedes
	// the response on the wire). A non-nil result replaces the status the
	// response reports.
	beforeRead func(c *fakeConn, id string) *ThreadStatus
	calls      map[string]int
	// failures[method] is how many upcoming requests of method are
	// answered with a JSON-RPC error instead of being handled.
	failures map[string]int
	conns    []*fakeConn

	// ready receives each connection once the client sent initialized.
	ready chan *fakeConn
}

type fakeConn struct {
	d       *fakeDaemon
	ws      *websocket.Conn
	writeMu sync.Mutex
	closed  chan struct{}
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	// Unix socket paths are length-limited; keep it short.
	dir, err := os.MkdirTemp("", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := &fakeDaemon{t: t, socket: filepath.Join(dir, "s.sock"), loaded: map[string]ThreadStatus{}, calls: map[string]int{}, failures: map[string]int{}, ready: make(chan *fakeConn, 16)}
	d.start()
	t.Cleanup(d.stop)
	return d
}

func (d *fakeDaemon) start() {
	ln, err := net.Listen("unix", d.socket)
	if err != nil {
		d.t.Fatal(err)
	}
	d.ln = ln
	d.srv = &http.Server{Handler: http.HandlerFunc(d.serve)}
	go func() { _ = d.srv.Serve(ln) }()
}

// stop closes the listener and every connection; the socket file is gone
// afterwards, like a stopped daemon.
func (d *fakeDaemon) stop() {
	if d.srv == nil {
		return
	}
	_ = d.srv.Close()
	d.srv = nil
	d.dropAll()
	_ = os.Remove(d.socket)
}

// dropAll force-closes every open connection while keeping the listener.
func (d *fakeDaemon) dropAll() {
	d.mu.Lock()
	conns := d.conns
	d.conns = nil
	d.mu.Unlock()
	for _, c := range conns {
		_ = c.ws.CloseNow()
	}
}

func (d *fakeDaemon) setThreads(threads ...Thread) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.threads = threads
}

func (d *fakeDaemon) setLoaded(id string, status ThreadStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if status.Type == statusNotLoaded {
		delete(d.loaded, id)
		return
	}
	d.loaded[id] = status
}

// failNext makes the next n requests of method fail with an
// application-level JSON-RPC error.
func (d *fakeDaemon) failNext(method string, n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failures[method] = n
}

func (d *fakeDaemon) callCount(method string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[method]
}

func (d *fakeDaemon) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/rpc" {
		http.NotFound(w, r)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c := &fakeConn{d: d, ws: ws, closed: make(chan struct{})}
	defer close(c.closed)
	d.mu.Lock()
	d.conns = append(d.conns, c)
	d.mu.Unlock()
	for {
		_, data, err := ws.Read(context.Background())
		if err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		d.mu.Lock()
		d.calls[msg.Method]++
		fail := len(msg.ID) > 0 && d.failures[msg.Method] > 0
		if fail {
			d.failures[msg.Method]--
		}
		d.mu.Unlock()
		if fail {
			c.send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32603, "message": "injected failure"}})
			continue
		}
		if len(msg.ID) == 0 {
			if msg.Method == "initialized" {
				d.ready <- c
			}
			continue
		}
		result, rpcErr := d.handle(c, msg.Method, msg.Params)
		reply := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		if rpcErr != nil {
			reply["error"] = map[string]any{"code": -32600, "message": rpcErr.Error()}
		} else {
			reply["result"] = result
		}
		c.send(reply)
	}
}

func (d *fakeDaemon) handle(c *fakeConn, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		return map[string]any{"codexHome": "/fake", "futureField": true}, nil
	case "thread/list":
		var p struct {
			Cursor *string `json:"cursor"`
		}
		_ = json.Unmarshal(params, &p)
		d.mu.Lock()
		defer d.mu.Unlock()
		start := 0
		if p.Cursor != nil {
			start, _ = strconv.Atoi(*p.Cursor)
		}
		end := len(d.threads)
		if d.pageSize > 0 && start+d.pageSize < end {
			end = start + d.pageSize
		}
		page := make([]Thread, 0, end-start)
		for _, t := range d.threads[start:end] {
			t.Status = ThreadStatus{Type: statusNotLoaded}
			if s, ok := d.loaded[t.ID]; ok {
				t.Status = s
			}
			page = append(page, t)
		}
		var next any
		if end < len(d.threads) {
			next = strconv.Itoa(end)
		}
		return map[string]any{"data": page, "nextCursor": next}, nil
	case "thread/loaded/list":
		d.mu.Lock()
		defer d.mu.Unlock()
		ids := make([]string, 0, len(d.loaded))
		for id := range d.loaded {
			ids = append(ids, id)
		}
		return map[string]any{"data": ids, "nextCursor": nil}, nil
	case "thread/read":
		var p struct {
			ThreadID     string `json:"threadId"`
			IncludeTurns bool   `json:"includeTurns"`
		}
		_ = json.Unmarshal(params, &p)
		if p.IncludeTurns {
			return nil, errors.New("observer must not read turns")
		}
		d.mu.Lock()
		hook := d.beforeRead
		d.mu.Unlock()
		var override *ThreadStatus
		if hook != nil {
			override = hook(c, p.ThreadID)
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		status, loaded := d.loaded[p.ThreadID]
		if !loaded {
			status = ThreadStatus{Type: statusNotLoaded}
		}
		if override != nil {
			status = *override
		}
		for _, t := range d.threads {
			if t.ID == p.ThreadID {
				t.Status = status
				return map[string]any{"thread": t}, nil
			}
		}
		if loaded {
			return map[string]any{"thread": Thread{ID: p.ThreadID, Status: status}}, nil
		}
		return nil, errors.New("thread not found")
	}
	return nil, errors.New("method not found")
}

func (c *fakeConn) send(msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		c.d.t.Error(err)
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.Write(context.Background(), websocket.MessageText, data)
}

func (c *fakeConn) notify(method string, params any) {
	c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// statusChanged sets id's status on the daemon and broadcasts it on c.
func (c *fakeConn) statusChanged(id string, status ThreadStatus) {
	c.d.setLoaded(id, status)
	c.notify(notifyStatusChanged, map[string]any{"threadId": id, "status": status})
}

func (d *fakeDaemon) waitReady(t *testing.T) *fakeConn {
	t.Helper()
	select {
	case c := <-d.ready:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no connection initialized")
		return nil
	}
}

// Test fixtures shared by the runtime/Observer tests.

func active(flags ...string) ThreadStatus {
	if flags == nil {
		flags = []string{}
	}
	return ThreadStatus{Type: statusActive, ActiveFlags: flags}
}

var (
	idle        = ThreadStatus{Type: statusIdle}
	notLoadedSt = ThreadStatus{Type: statusNotLoaded}
)

func catalogThread(id string, createdAt int64) Thread {
	return Thread{ID: id, Name: ptr(id), CWD: "/work", CreatedAt: createdAt, UpdatedAt: createdAt}
}

func titleThread(id string) Thread {
	return Thread{ID: id, Ephemeral: true, ThreadSource: ptr(threadSourceTitle), CWD: "/work"}
}

// newObservedProvider returns a Provider observing d with test timings. Its
// writer probe reports every thread free unless listed in writers, and
// counts calls per thread.
type writerProbe struct {
	mu      sync.Mutex
	writers map[string]bool
	calls   map[string]int
}

func (w *writerProbe) free(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls[id]++
	return !w.writers[id]
}

func (w *writerProbe) callsFor(id string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[id]
}

func newObservedProvider(t *testing.T, d *fakeDaemon, api *fakeAPI) (*Provider, *writerProbe) {
	t.Helper()
	if api == nil {
		api = &fakeAPI{}
	}
	probe := &writerProbe{writers: map[string]bool{}, calls: map[string]int{}}
	p := &Provider{API: api, Store: localstate.New(filepath.Join(t.TempDir(), "state.json")), writerFree: probe.free}
	socket := d.socket
	rt := newCodexRuntime(func() (string, error) { return socket, nil })
	rt.minBackoff, rt.maxBackoff, rt.catalogGap = 10*time.Millisecond, 50*time.Millisecond, 0
	p.obs.rt = rt
	return p, probe
}

// observe subscribes to p until the test ends and returns the channel.
func observe(t *testing.T, p *Provider) <-chan sessionctl.ProviderUpdate {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ch := p.Observe(ctx)
	t.Cleanup(func() {
		cancel()
		for range ch {
		}
		p.obs.mu.Lock()
		stopped := p.obs.stopped
		p.obs.mu.Unlock()
		<-stopped
	})
	return ch
}

// waitFor returns the first update satisfying ok, failing after a timeout.
// Every update seen on the way is passed to seen (if non-nil).
func waitFor(t *testing.T, ch <-chan sessionctl.ProviderUpdate, what string, ok func(sessionctl.ProviderUpdate) bool, seen ...func(sessionctl.ProviderUpdate)) sessionctl.ProviderUpdate {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case u, open := <-ch:
			if !open {
				t.Fatalf("observer closed while waiting for %s", what)
			}
			for _, f := range seen {
				f(u)
			}
			if ok(u) {
				return u
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func rowOf(u sessionctl.ProviderUpdate, id string) (session.Session, bool) {
	for _, s := range u.Sessions {
		if s.Key.ID == id {
			return s, true
		}
	}
	return session.Session{}, false
}

// activityIs reports whether u is a successful snapshot where id has want.
func activityIs(id string, want session.Activity) func(sessionctl.ProviderUpdate) bool {
	return func(u sessionctl.ProviderUpdate) bool {
		s, ok := rowOf(u, id)
		return u.Err == nil && ok && s.Activity == want
	}
}
