package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Defaults for codexRuntime's timing; tests shorten them.
const (
	defaultMinBackoff = 500 * time.Millisecond
	defaultMaxBackoff = 30 * time.Second
	// defaultCatalogGap is the minimum spacing between two catalog resyncs
	// a connection triggers by itself, so a burst of catalog-changing
	// notifications costs one thread/list walk rather than one each.
	defaultCatalogGap = time.Second
	// maxSnapshotRounds bounds how often a snapshot re-reads threads whose
	// status changed while it was being taken before giving up on this
	// connection (and retrying with backoff) rather than installing a
	// status it could not confirm.
	maxSnapshotRounds = 16
)

// codexRuntime observes the shared Codex app-server daemon over one
// persistent connection (see rpcConn) and keeps what it learned: the
// thread catalog and the native status of every thread the daemon has
// loaded. Before every connection attempt it requests a ready endpoint from
// its lifecycle-independent dependency; an unavailable endpoint or daemon is
// an ordinary state it retries with bounded backoff.
//
// Concurrency: network I/O happens only on the run goroutine (dial,
// snapshot, catalog resync) and on the connection's reader (which only
// delivers notifications into handleNotification). mu guards the observed
// state and is never held across I/O. Every change a consumer can see is
// signalled on changed, a one-slot channel, so a slow consumer only ever
// rebuilds from the latest state.
type codexRuntime struct {
	readySocket func(context.Context) (string, error)

	minBackoff, maxBackoff time.Duration
	catalogGap             time.Duration

	changed chan struct{}
	wake    chan struct{}

	mu sync.Mutex
	// fetchSeq numbers catalog fetches in the order they start; catalogSeq
	// is the number of the fetch that produced catalog. A fetch that
	// started before the installed one never replaces it.
	fetchSeq   uint64
	catalog    []Thread
	catalogSeq uint64
	listed     map[string]bool
	// status holds the native status of each daemon-loaded thread; a
	// thread absent from it is notLoaded.
	status map[string]ThreadStatus
	// hidden remembers threads known to be internal temporary threads, so
	// their notifications never trigger a catalog resync.
	hidden map[string]bool
	// live: a snapshot is installed and its connection is up. everLive:
	// that has happened at least once in the current lifecycle (see
	// startLifecycle).
	live, everLive bool
	lastErr        error
	lastReadyErr   bool
	// While snapshotting, status notifications only mark their thread
	// dirty; the snapshot re-reads it before installing.
	snapshotting bool
	dirty        map[string]bool
	needCatalog  bool
}

func newCodexRuntime(readySocket func(context.Context) (string, error)) *codexRuntime {
	return &codexRuntime{
		readySocket: readySocket,
		minBackoff:  defaultMinBackoff,
		maxBackoff:  defaultMaxBackoff,
		catalogGap:  defaultCatalogGap,
		changed:     make(chan struct{}, 1),
		wake:        make(chan struct{}, 1),
		hidden:      map[string]bool{},
	}
}

// runtimeView is a consistent copy of the observed state.
type runtimeView struct {
	catalog      []Thread
	status       map[string]ThreadStatus
	live         bool
	everLive     bool
	lastErr      error
	lastReadyErr bool
}

// observe resolves one catalog thread's observation from this view: a
// thread cannot be observed at all while the connection is down;
// otherwise its daemon status, notLoaded when the daemon does not have it
// loaded (see observeThread).
func (v runtimeView) observe(t Thread, writerFree func() bool) observation {
	if !v.live {
		return unobserved
	}
	status, loaded := v.status[t.ID]
	if !loaded {
		status = ThreadStatus{Type: statusNotLoaded}
	}
	return observeThread(status, writerFree)
}

func (r *codexRuntime) view() runtimeView {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := make(map[string]ThreadStatus, len(r.status))
	for id, s := range r.status {
		status[id] = s
	}
	return runtimeView{catalog: r.catalog, status: status, live: r.live, everLive: r.everLive, lastErr: r.lastErr, lastReadyErr: r.lastReadyErr}
}

// kick signals changed without blocking.
func (r *codexRuntime) kick() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}

// beginCatalogFetch numbers a catalog fetch about to start; pass the
// number to installCatalog with its result.
func (r *codexRuntime) beginCatalogFetch() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fetchSeq++
	return r.fetchSeq
}

// installCatalog replaces the catalog with threads fetched by fetch seq,
// unless a fetch that started later has already been installed -- then
// nothing changes, the status cache included. It reports whether the
// catalog was replaced. Any source may install --
// the persistent connection or Provider.List's short-lived app-server --
// since both read the same native thread store.
func (r *codexRuntime) installCatalog(seq uint64, threads []Thread) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.installCatalogLocked(seq, threads)
}

func (r *codexRuntime) installCatalogLocked(seq uint64, threads []Thread) bool {
	if seq <= r.catalogSeq {
		return false
	}
	catalog := make([]Thread, 0, len(threads))
	listed := make(map[string]bool, len(threads))
	for _, t := range threads {
		if hiddenThread(t) {
			r.hidden[t.ID] = true
			continue
		}
		catalog = append(catalog, t)
		listed[t.ID] = true
	}
	// A thread the replaced catalog listed but this one does not has left
	// the catalog (archived, deleted, ...): its cached status is dropped
	// with it, so if the thread comes back without a new status
	// notification (e.g. unarchived, never loaded again) it is notLoaded,
	// not whatever the daemon reported before it left. A status for a
	// thread no catalog has listed yet (a new thread not materialized in
	// thread/list) is kept; a later catalog that lists it needs it.
	for id := range r.listed {
		if !listed[id] {
			delete(r.status, id)
		}
	}
	r.catalog, r.catalogSeq, r.listed = catalog, seq, listed
	return true
}

// requestCatalog asks the live connection to re-read the catalog.
func (r *codexRuntime) requestCatalog() {
	r.mu.Lock()
	r.needCatalog = true
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// startLifecycle resets per-lifecycle state before run starts: a new
// lifecycle earns its authority (everLive) from its own connection.
func (r *codexRuntime) startLifecycle() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.live, r.everLive, r.lastErr, r.lastReadyErr, r.status = false, false, nil, false, nil
}

// run connects, and reconnects with bounded backoff, until ctx ends.
func (r *codexRuntime) run(ctx context.Context) {
	delay := r.minBackoff
	for {
		wasLive, endpointReady, err := r.connect(ctx)
		r.disconnected(err, !endpointReady)
		if ctx.Err() != nil {
			return
		}
		if wasLive {
			delay = r.minBackoff
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(delay*2, r.maxBackoff)
	}
}

// disconnected drops everything only a live connection can vouch for. It
// signals a change only when there is one (the connection was live, or the
// failure differs), so a long outage does not republish every retry.
func (r *codexRuntime) disconnected(err error, readyErr bool) {
	if err == nil {
		err = errConnClosed
	}
	r.mu.Lock()
	changed := r.live || r.lastErr == nil || r.lastErr.Error() != err.Error() || r.lastReadyErr != readyErr
	r.live, r.snapshotting, r.dirty, r.status = false, false, nil, nil
	r.lastErr, r.lastReadyErr = err, readyErr
	r.mu.Unlock()
	if changed {
		r.kick()
	}
}

// connect runs one connection: handshake, snapshot, then catalog resyncs on
// demand until the connection or ctx ends. wasLive reports whether the
// snapshot was installed.
func (r *codexRuntime) connect(ctx context.Context) (wasLive, endpointReady bool, err error) {
	socket, err := r.readySocket(ctx)
	if err != nil {
		return false, false, err
	}
	conn, err := dialRPC(ctx, socket, r.handleNotification)
	if err != nil {
		return false, true, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, conn.Close)
	defer stop()

	if err := initializeRPC(ctx, conn.call, conn.notify, nil); err != nil {
		return false, true, err
	}
	if err := r.snapshot(ctx, conn); err != nil {
		return false, true, err
	}
	r.kick()
	return true, true, r.serveCatalog(ctx, conn)
}

// snapshot rebuilds the observed state from scratch: the catalog
// (thread/list), the loaded threads (thread/loaded/list) and each one's
// status (thread/read). Any failed request fails the whole snapshot; only
// a native notLoaded status makes a thread notLoaded. A status notification that arrives meanwhile only
// marks its thread dirty; the thread is read again, as often as needed,
// before the snapshot installs -- a read response is never assumed to be
// newer or older than a notification. A thread loaded before but missing
// now is simply absent, i.e. notLoaded.
func (r *codexRuntime) snapshot(ctx context.Context, conn *rpcConn) error {
	r.mu.Lock()
	r.snapshotting, r.dirty, r.needCatalog = true, map[string]bool{}, false
	r.fetchSeq++
	seq := r.fetchSeq
	r.mu.Unlock()

	threads, err := listThreads(ctx, conn.call, false)
	if err != nil {
		return err
	}
	ids, err := loadedThreadIDs(ctx, conn)
	if err != nil {
		return err
	}
	status := map[string]ThreadStatus{}
	var hidden []string
	read := func(id string) error {
		t, err := readThread(ctx, conn, id)
		switch {
		case err != nil:
			// Any failure -- transport or an app-server error, even one
			// caused by the thread unloading since thread/loaded/list --
			// leaves the status unestablished. It is never guessed to be
			// notLoaded (which could publish a false Idle): the snapshot
			// fails and is retaken on a new connection.
			return fmt.Errorf("thread/read %s: %w", id, err)
		case hiddenThread(t):
			hidden = append(hidden, id)
			delete(status, id)
		case t.Status.Type == statusNotLoaded:
			delete(status, id)
		default:
			status[id] = t.Status
		}
		return nil
	}
	for _, id := range ids {
		if err := read(id); err != nil {
			return err
		}
	}
	for round := 0; ; round++ {
		dirty := r.installSnapshot(seq, threads, status, hidden)
		if len(dirty) == 0 {
			return nil
		}
		if round >= maxSnapshotRounds {
			return errors.New("codex thread status kept changing during snapshot")
		}
		for id := range dirty {
			if err := read(id); err != nil {
				return err
			}
		}
	}
}

// installSnapshot installs a finished snapshot and goes live, unless a
// status notification arrived since the reads: then it returns the dirty
// threads for the caller to read again and installs nothing.
func (r *codexRuntime) installSnapshot(seq uint64, threads []Thread, status map[string]ThreadStatus, hidden []string) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.dirty) > 0 {
		dirty := r.dirty
		r.dirty = map[string]bool{}
		return dirty
	}
	for _, id := range hidden {
		r.hidden[id] = true
	}
	r.installCatalogLocked(seq, threads)
	installed := make(map[string]ThreadStatus, len(status))
	for id, s := range status {
		if !r.hidden[id] {
			installed[id] = s
		}
	}
	r.status = installed
	r.snapshotting, r.dirty = false, nil
	r.live, r.everLive, r.lastErr, r.lastReadyErr = true, true, nil, false
	return nil
}

// serveCatalog re-reads the catalog whenever a notification asked for it
// (see requestCatalog), at most once per catalogGap, until the connection
// or ctx ends. A failed re-read ends serving with its error: the catalog
// is then known to be stale, and the caller abandons the connection so a
// fresh snapshot replaces it, rather than keeping the stale catalog until
// some later notification happens to ask again. It returns nil only when
// ctx ended.
func (r *codexRuntime) serveCatalog(ctx context.Context, conn *rpcConn) error {
	var last time.Time
	for {
		r.mu.Lock()
		need := r.needCatalog
		r.needCatalog = false
		r.mu.Unlock()
		if !need {
			select {
			case <-r.wake:
				continue
			case <-conn.Done():
				return conn.Err()
			case <-ctx.Done():
				return nil
			}
		}
		if wait := r.catalogGap - time.Since(last); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-conn.Done():
				timer.Stop()
				return conn.Err()
			case <-ctx.Done():
				timer.Stop()
				return nil
			}
		}
		last = time.Now()
		seq := r.beginCatalogFetch()
		threads, err := listThreads(ctx, conn.call, false)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("catalog resync: %w", err)
		}
		if r.installCatalog(seq, threads) {
			r.kick()
		}
	}
}

// Notification methods the runtime reacts to. Every one is a broadcast;
// the runtime holds no thread subscription and ignores subscriber-only
// messages (turn/*, approvals, user input).
const (
	notifyStatusChanged = "thread/status/changed"
	notifyClosed        = "thread/closed"
	notifyStarted       = "thread/started"
	notifyNameUpdated   = "thread/name/updated"
	notifyArchived      = "thread/archived"
	notifyUnarchived    = "thread/unarchived"
	notifyDeleted       = "thread/deleted"
)

// handleNotification applies one broadcast notification. Status changes
// (including a thread closing, which unloads it) update the status cache
// directly; anything that changes the catalog's shape or metadata asks for
// a native catalog re-read instead of being merged by hand. Runs on the
// connection's reader goroutine, so it never performs I/O.
func (r *codexRuntime) handleNotification(method string, params json.RawMessage) {
	switch method {
	case notifyStatusChanged:
		var p struct {
			ThreadID string       `json:"threadId"`
			Status   ThreadStatus `json:"status"`
		}
		if json.Unmarshal(params, &p) != nil || p.ThreadID == "" {
			return
		}
		r.setStatus(p.ThreadID, p.Status)
	case notifyClosed:
		var p struct {
			ThreadID string `json:"threadId"`
		}
		if json.Unmarshal(params, &p) != nil || p.ThreadID == "" {
			return
		}
		r.setStatus(p.ThreadID, ThreadStatus{Type: statusNotLoaded})
	case notifyStarted:
		var p struct {
			Thread Thread `json:"thread"`
		}
		if json.Unmarshal(params, &p) != nil || p.Thread.ID == "" {
			return
		}
		if hiddenThread(p.Thread) {
			r.mu.Lock()
			r.hidden[p.Thread.ID] = true
			r.mu.Unlock()
			return
		}
		r.requestCatalog()
	case notifyNameUpdated, notifyArchived, notifyUnarchived, notifyDeleted:
		r.requestCatalog()
	}
}

// setStatus records one thread's new native status. During a snapshot it
// only marks the thread dirty (see snapshot). A status for a thread the
// catalog does not list yet (and that is not known to be hidden) also asks
// for a catalog re-read, since a newly materialized thread would otherwise
// stay out of the catalog.
func (r *codexRuntime) setStatus(id string, status ThreadStatus) {
	r.mu.Lock()
	if r.snapshotting {
		r.dirty[id] = true
		r.mu.Unlock()
		return
	}
	if !r.live || r.hidden[id] {
		r.mu.Unlock()
		return
	}
	if status.Type == statusNotLoaded {
		delete(r.status, id)
	} else {
		if r.status == nil {
			r.status = map[string]ThreadStatus{}
		}
		r.status[id] = status
	}
	unlisted := !r.listed[id]
	r.mu.Unlock()
	r.kick()
	if unlisted {
		r.requestCatalog()
	}
}

// loadedThreadIDs walks every thread/loaded/list page.
func loadedThreadIDs(ctx context.Context, conn *rpcConn) ([]string, error) {
	var ids []string
	var cursor *string
	seen := map[string]bool{}
	for {
		var res struct {
			Data []string `json:"data"`
			Next *string  `json:"nextCursor"`
		}
		params := map[string]any{}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		if err := conn.call(ctx, "thread/loaded/list", params, &res); err != nil {
			return nil, err
		}
		ids = append(ids, res.Data...)
		if res.Next == nil || *res.Next == "" {
			return ids, nil
		}
		if seen[*res.Next] {
			return nil, errors.New("thread/loaded/list repeated cursor")
		}
		seen[*res.Next] = true
		cursor = res.Next
	}
}

// readThread reads one thread's current metadata and status, without
// turns.
func readThread(ctx context.Context, conn *rpcConn, id string) (Thread, error) {
	var res struct {
		Thread Thread `json:"thread"`
	}
	err := conn.call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &res)
	return res.Thread, err
}
