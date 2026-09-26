package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tingtt/agentsctl/internal/session"
	"github.com/tingtt/agentsctl/internal/sessionctl"
)

// fakeDaemon is a minimal scripted Codex app-server listening on a Unix
// socket with the WebSocket transport: initialize, thread/list,
// thread/loaded/list, thread/read, thread/turns/list, the Dispatch requests
// (thread/start, turn/start, thread/unsubscribe, thread/name/set), exact
// turn interruption, server-sent notifications and requests, and forced
// disconnects. It implements only what the provider uses.
type fakeDaemon struct {
	t      *testing.T
	socket string
	ln     net.Listener
	srv    *http.Server

	mu      sync.Mutex
	threads []Thread // thread/list: the durable catalog
	// pending holds threads thread/start created whose rollout is not
	// persisted yet: loaded and readable, but not in thread/list, and
	// thread/name/set on them fails, like the real daemon. A thread moves
	// to threads when its first turn/started goes out (see materialize).
	pending         map[string]Thread
	archived        []Thread                // moved here by thread/archive
	loaded          map[string]ThreadStatus // thread/loaded/list + thread/read
	activeTurns     map[string]string       // current inProgress turn by thread
	pendingRequests map[string]bool         // approval/user-input request by thread
	pageSize        int                     // thread/list page size; 0 = one page
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

	// events logs, in arrival order, the method of every client request
	// (not notifications) and "close" whenever a connection ends. params
	// holds each request's params by method, in arrival order.
	events []string
	params map[string][]json.RawMessage
	// clientResponses holds every JSON-RPC response a client sent, i.e.
	// every answer to a server-initiated request.
	clientResponses []json.RawMessage
	// dropOn[method] closes the connection instead of answering a request
	// of method; hangOn[method] never answers it.
	dropOn map[string]bool
	hangOn map[string]bool

	// Dispatch scripting. threadSeq numbers the threads thread/start
	// creates (thread-new-<n>). threadStartResult / turnStartResult, when
	// set, replace the respective response. unsubscribeStatus is the
	// thread/unsubscribe status (default unsubscribed). before[method]
	// runs on the connection's handler goroutine before a request of
	// method is answered, so whatever it sends precedes the response.
	threadSeq         int
	threadStartResult any
	turnStartResult   any
	unsubscribeStatus any
	before            map[string]func(c *fakeConn)

	// Turn lifecycle. A thread thread/start created stays pending until
	// its turn/started goes out; turnStarted says when:
	//   ""               after the response, but only once the client's next
	//                    request was handled (or turnStartedGrace passed),
	//                    so a client that does not wait for it races it
	//   "beforeResponse" before the turn/start response
	//   "never"          not at all
	//   "drop"           never; the connection closes instead
	// unrelatedTurnStarted sends turn/started for another thread and for
	// another turn of the same thread first. traceLifecycle logs every
	// turn/started sent into events as "turn/started <thread>/<turn>".
	turnStarted          string
	unrelatedTurnStarted bool
	traceLifecycle       bool
	// turnInterruptCompletion controls notification ordering. Empty sends
	// turn/completed before the response; "afterResponse" defers it until
	// the response has been written.
	turnInterruptCompletion string

	// ready receives each connection once the client sent initialized.
	ready chan *fakeConn
}

type fakeConn struct {
	d       *fakeDaemon
	ws      *websocket.Conn
	writeMu sync.Mutex
	closed  chan struct{}

	// held is lifecycle work (see holdTurnStarted) waiting to run after the
	// connection's next request has been handled, or after
	// turnStartedGrace if none comes.
	heldMu     sync.Mutex
	held       []func()
	afterReply []func()
}

// turnStartedGrace is how long a held turn/started waits for a request
// that would have to be handled before it. Only a client that sends
// nothing while waiting for turn/started ever waits it out.
const turnStartedGrace = 50 * time.Millisecond

// hold defers f until the connection's next request has been handled (see
// serve), or until turnStartedGrace passes without one.
func (c *fakeConn) hold(f func()) {
	c.heldMu.Lock()
	c.held = append(c.held, f)
	c.heldMu.Unlock()
	time.AfterFunc(turnStartedGrace, func() { c.release(c.takeHeld()) })
}

func (c *fakeConn) takeHeld() []func() {
	c.heldMu.Lock()
	defer c.heldMu.Unlock()
	held := c.held
	c.held = nil
	return held
}

func (c *fakeConn) release(held []func()) {
	for _, f := range held {
		f()
	}
}

func (c *fakeConn) afterResponse(f func()) {
	c.heldMu.Lock()
	c.afterReply = append(c.afterReply, f)
	c.heldMu.Unlock()
}

func (c *fakeConn) takeAfterResponse() []func() {
	c.heldMu.Lock()
	defer c.heldMu.Unlock()
	after := c.afterReply
	c.afterReply = nil
	return after
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	// Unix socket paths are length-limited; keep it short.
	dir, err := os.MkdirTemp("", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	d := &fakeDaemon{t: t, socket: filepath.Join(dir, "s.sock"), loaded: map[string]ThreadStatus{}, activeTurns: map[string]string{}, pendingRequests: map[string]bool{}, calls: map[string]int{}, failures: map[string]int{}, params: map[string][]json.RawMessage{}, pending: map[string]Thread{}, before: map[string]func(*fakeConn){}, dropOn: map[string]bool{}, hangOn: map[string]bool{}, ready: make(chan *fakeConn, 16)}
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
	srv := &http.Server{Handler: http.HandlerFunc(d.serve)}
	d.srv = srv
	// srv, not d.srv: stop may already have cleared d.srv when this runs.
	go func() { _ = srv.Serve(ln) }()
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

func (d *fakeDaemon) activeTurn(id string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.activeTurns[id]
}

func (d *fakeDaemon) loadedStatus(id string) ThreadStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.loaded[id]
}

func (d *fakeDaemon) setPendingRequest(id string, pending bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pendingRequests[id] = pending
}

func (d *fakeDaemon) hasPendingRequest(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pendingRequests[id]
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
	defer func() {
		d.mu.Lock()
		d.events = append(d.events, "close")
		d.mu.Unlock()
	}()
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
		if msg.Method == "" {
			d.mu.Lock()
			d.clientResponses = append(d.clientResponses, append(json.RawMessage(nil), data...))
			d.mu.Unlock()
			continue
		}
		var held []func()
		if len(msg.ID) > 0 {
			held = c.takeHeld()
		}
		d.mu.Lock()
		d.calls[msg.Method]++
		if len(msg.ID) > 0 {
			d.events = append(d.events, msg.Method)
			d.params[msg.Method] = append(d.params[msg.Method], msg.Params)
		}
		drop, hang := d.dropOn[msg.Method], d.hangOn[msg.Method]
		fail := len(msg.ID) > 0 && d.failures[msg.Method] > 0
		if fail {
			d.failures[msg.Method]--
		}
		d.mu.Unlock()
		if len(msg.ID) > 0 && drop {
			_ = ws.CloseNow()
			return
		}
		if len(msg.ID) > 0 && hang {
			c.release(held)
			continue
		}
		if fail {
			c.send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32603, "message": "injected failure"}})
			c.release(held)
			continue
		}
		if len(msg.ID) == 0 {
			if msg.Method == "initialized" {
				select {
				case d.ready <- c:
				default: // nobody waits for this connection (e.g. Dispatch's own)
				}
			}
			continue
		}
		d.mu.Lock()
		hook := d.before[msg.Method]
		d.mu.Unlock()
		if hook != nil {
			hook(c)
		}
		result, rpcErr := d.handle(c, msg.Method, msg.Params)
		reply := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		if rpcErr != nil {
			reply["error"] = map[string]any{"code": -32600, "message": rpcErr.Error()}
		} else {
			reply["result"] = result
		}
		c.send(reply)
		if msg.Method == "turn/interrupt" {
			d.mu.Lock()
			if d.traceLifecycle {
				d.events = append(d.events, "turn/interrupt response")
			}
			d.mu.Unlock()
		}
		c.release(c.takeAfterResponse())
		c.release(held)
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
		if t, ok := d.pending[p.ThreadID]; ok {
			t.Status = status
			return map[string]any{"thread": t}, nil
		}
		if loaded {
			return map[string]any{"thread": Thread{ID: p.ThreadID, Status: status}}, nil
		}
		return nil, errors.New("thread not found")
	case "thread/turns/list":
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(params, &p)
		d.mu.Lock()
		defer d.mu.Unlock()
		if _, ok := d.pending[p.ThreadID]; ok {
			return nil, fmt.Errorf("thread %s is not materialized yet; %s", p.ThreadID, turnsListUnmaterializedMessage)
		}
		var turns []any
		if turnID := d.activeTurns[p.ThreadID]; turnID != "" {
			turns = append(turns, map[string]any{"id": turnID, "status": turnStatusInProgress, "items": []any{}})
		}
		return map[string]any{"data": turns, "nextCursor": nil}, nil
	case "thread/start":
		var p struct {
			CWD string `json:"cwd"`
		}
		_ = json.Unmarshal(params, &p)
		d.mu.Lock()
		if d.threadStartResult != nil {
			defer d.mu.Unlock()
			return d.threadStartResult, nil
		}
		d.threadSeq++
		t := Thread{ID: "thread-new-" + strconv.Itoa(d.threadSeq), CWD: p.CWD, CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix()}
		d.pending[t.ID] = t
		d.loaded[t.ID] = idle
		d.mu.Unlock()
		t.Status = idle
		d.broadcast(notifyStarted, map[string]any{"thread": t})
		return map[string]any{"thread": t, "model": "fake-model", "modelProvider": "fake", "cwd": p.CWD}, nil
	case "turn/start":
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(params, &p)
		d.mu.Lock()
		override, mode, unrelated := d.turnStartResult, d.turnStarted, d.unrelatedTurnStarted
		d.mu.Unlock()
		if override != nil {
			return override, nil
		}
		d.setLoaded(p.ThreadID, active())
		d.mu.Lock()
		d.activeTurns[p.ThreadID] = "turn-1"
		d.mu.Unlock()
		d.broadcast(notifyStatusChanged, map[string]any{"threadId": p.ThreadID, "status": active()})
		if unrelated {
			c.turnStarted("other-thread", "turn-1", false)
			c.turnStarted(p.ThreadID, "turn-other", false)
		}
		switch mode {
		case "":
			c.hold(func() { c.turnStarted(p.ThreadID, "turn-1", true) })
		case "beforeResponse":
			c.turnStarted(p.ThreadID, "turn-1", true)
		case "drop":
			c.hold(func() { _ = c.ws.CloseNow() })
		}
		return map[string]any{"turn": map[string]any{"id": "turn-1", "status": "inProgress", "items": []any{}}}, nil
	case "turn/interrupt":
		var p struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
		}
		_ = json.Unmarshal(params, &p)
		d.mu.Lock()
		activeTurnID := d.activeTurns[p.ThreadID]
		mode := d.turnInterruptCompletion
		d.mu.Unlock()
		if activeTurnID == "" {
			return nil, errors.New("no active turn to interrupt")
		}
		if activeTurnID != p.TurnID {
			return nil, fmt.Errorf("expected active turn id %s but found %s", p.TurnID, activeTurnID)
		}
		complete := func() { d.completeTurn(c, p.ThreadID, p.TurnID, turnStatusInterrupted) }
		if mode == "afterResponse" {
			c.afterResponse(complete)
		} else {
			complete()
		}
		return map[string]any{}, nil
	case "thread/unsubscribe":
		d.mu.Lock()
		defer d.mu.Unlock()
		status := d.unsubscribeStatus
		if status == nil {
			status = "unsubscribed"
		}
		return map[string]any{"status": status}, nil
	case "thread/archive", "thread/unarchive":
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(params, &p)
		from, to, note := &d.threads, &d.archived, notifyArchived
		if method == "thread/unarchive" {
			from, to, note = &d.archived, &d.threads, notifyUnarchived
		}
		d.mu.Lock()
		i := slices.IndexFunc(*from, func(t Thread) bool { return t.ID == p.ThreadID })
		if i < 0 {
			d.mu.Unlock()
			return nil, errors.New("thread not found")
		}
		t := (*from)[i]
		*from = slices.Delete(*from, i, i+1)
		*to = append(*to, t)
		// Archiving tears a loaded thread down; unarchiving loads nothing.
		delete(d.loaded, p.ThreadID)
		d.mu.Unlock()
		d.broadcast(note, map[string]any{"threadId": p.ThreadID})
		if method == "thread/unarchive" {
			t.Status = notLoadedSt
			return map[string]any{"thread": t}, nil
		}
		return map[string]any{}, nil
	case "thread/name/set":
		var p struct {
			ThreadID string `json:"threadId"`
			Name     string `json:"name"`
		}
		_ = json.Unmarshal(params, &p)
		d.mu.Lock()
		if _, ok := d.pending[p.ThreadID]; ok {
			d.mu.Unlock()
			return nil, errors.New("failed to set thread name: Fatal error: failed to update thread metadata " + p.ThreadID)
		}
		for i := range d.threads {
			if d.threads[i].ID == p.ThreadID {
				d.threads[i].Name = ptr(p.Name)
			}
		}
		d.mu.Unlock()
		d.broadcast(notifyNameUpdated, map[string]any{"threadId": p.ThreadID, "threadName": p.Name})
		return map[string]any{}, nil
	}
	return nil, errors.New("method not found")
}

func (d *fakeDaemon) completeTurn(c *fakeConn, threadID, turnID, status string) {
	d.mu.Lock()
	if d.activeTurns[threadID] != turnID {
		d.mu.Unlock()
		return
	}
	delete(d.activeTurns, threadID)
	delete(d.pendingRequests, threadID)
	d.loaded[threadID] = idle
	if d.traceLifecycle {
		d.events = append(d.events, "turn/completed "+threadID+"/"+turnID+" "+status)
	}
	d.mu.Unlock()
	c.notify("turn/completed", map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": status, "items": []any{}}})
	d.broadcast(notifyStatusChanged, map[string]any{"threadId": threadID, "status": idle})
}

func (d *fakeDaemon) startActiveTurn(threadID, turnID string, status ThreadStatus) {
	d.mu.Lock()
	d.activeTurns[threadID] = turnID
	d.loaded[threadID] = status
	d.mu.Unlock()
}

func (d *fakeDaemon) startUnmaterializedTurn(threadID, turnID string, status ThreadStatus) {
	d.mu.Lock()
	d.pending[threadID] = Thread{ID: threadID, CWD: "/work", CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix()}
	d.activeTurns[threadID] = turnID
	d.loaded[threadID] = status
	d.mu.Unlock()
}

// turnStarted sends turn/started for threadID's turnID on c. materializes
// marks the thread's rollout persisted first, as the real daemon does
// before it sends the notification.
func (c *fakeConn) turnStarted(threadID, turnID string, materializes bool) {
	d := c.d
	d.mu.Lock()
	if materializes {
		d.materializeLocked(threadID)
	}
	if d.traceLifecycle {
		d.events = append(d.events, "turn/started "+threadID+"/"+turnID)
	}
	d.mu.Unlock()
	c.notify("turn/started", map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "inProgress", "items": []any{}}})
}

// materialize persists a pending thread: from now on thread/list lists it.
func (d *fakeDaemon) materialize(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.materializeLocked(id)
}

func (d *fakeDaemon) materializeLocked(id string) {
	if t, ok := d.pending[id]; ok {
		delete(d.pending, id)
		d.threads = append(d.threads, t)
	}
}

// broadcast sends a notification on every open connection.
func (d *fakeDaemon) broadcast(method string, params any) {
	d.mu.Lock()
	conns := append([]*fakeConn(nil), d.conns...)
	d.mu.Unlock()
	for _, c := range conns {
		c.notify(method, params)
	}
}

// requestsOf returns the params of every request of method so far.
func (d *fakeDaemon) requestsOf(method string) []json.RawMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]json.RawMessage(nil), d.params[method]...)
}

func (d *fakeDaemon) eventLog() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.events...)
}

func (d *fakeDaemon) responses() []json.RawMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]json.RawMessage(nil), d.clientResponses...)
}

// waitClosed waits until n connections have ended.
func (d *fakeDaemon) waitClosed(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		closed := 0
		for _, e := range d.eventLog() {
			if e == "close" {
				closed++
			}
		}
		if closed >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections closed, want %d", closed, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
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
	p := &Provider{API: api, ControlSocket: d.socket, writerFree: probe.free}
	rt := p.runtime()
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
