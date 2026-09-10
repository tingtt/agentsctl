//go:build darwin || linux

package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/tingtt/agentsctl/internal/localstate"
	processinfo "github.com/tingtt/agentsctl/internal/process"
	"github.com/tingtt/agentsctl/internal/supervisor/protocol"
	"golang.org/x/sys/unix"
)

type Request struct {
	Action      string            `json:"action"`
	RunID       string            `json:"runId,omitempty"`
	SessionID   string            `json:"sessionId,omitempty"`
	Args        []string          `json:"args,omitempty"`
	CWD         string            `json:"cwd,omitempty"`
	Provider    string            `json:"provider,omitempty"`
	Baseline    []string          `json:"baseline,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
}
type Response struct {
	OK               bool            `json:"ok"`
	Error            string          `json:"error,omitempty"`
	Run              *localstate.Run `json:"run,omitempty"`
	ProtocolVersion  int             `json:"protocolVersion,omitempty"`
	BuildVersion     string          `json:"buildVersion,omitempty"`
	DaemonPID        int             `json:"daemonPid,omitempty"`
	DaemonStartTime  uint64          `json:"daemonStartTime,omitempty"`
	DaemonUID        uint32          `json:"daemonUid,omitempty"`
	DaemonExecutable string          `json:"daemonExecutable,omitempty"`
	DaemonParentPID  int             `json:"daemonParentPid,omitempty"`
	DaemonCWD        string          `json:"daemonCwd,omitempty"`
	DaemonPATH       string          `json:"daemonPath,omitempty"`
	ResolvedPath     string          `json:"resolvedPath,omitempty"`
	Output           string          `json:"output,omitempty"`
}

const ProtocolVersion = 3

// BuildVersion changed here to attach-backpressure-2026-09-10: the wire
// format itself (frame layout, Request/Response shapes, the Failure frame
// kind) is unchanged from the previous build, so ProtocolVersion did not
// move -- but subscriber.writeTo's backpressure, disconnect, and error-
// reporting behavior changed enough (see issue #31) that a client built
// against this behavior must never keep talking to an already-running
// daemon still running the old implementation. See compatible and
// Client.restartOwned: an incompatible owned daemon with no active
// managed run is restarted automatically; one with an active run is left
// alone rather than silently discarding it.
const BuildVersion = "attach-backpressure-2026-09-10"

type process struct {
	run          localstate.Run
	cmd          *exec.Cmd
	ptmx         *os.File
	mu           sync.Mutex
	resizeMu     sync.Mutex
	subscribers  map[*subscriber]struct{}
	done         chan struct{}
	editorRedraw *externalEditorRedrawDetector
}

// subscriberMaxBufferedBytes bounds how much PTY output a single attach
// subscriber may have outstanding -- queued in buf *and* already handed
// to writeTo but not yet confirmed delivered (see inFlight) -- before it
// is judged too slow to keep and is disconnected. The bound is on total
// bytes, never on the number of PTY read() chunks: a burst of many small
// writes (e.g. a Codex redraw made of many short escape sequences) must
// not exhaust a chunk-counted queue while the underlying byte volume is
// still trivial -- that chunk-vs-byte mismatch was the root cause of
// issue #31. 4 MiB comfortably absorbs realistic full-screen redraws many
// times over while keeping the worst-case per-subscriber memory cost
// small and fixed.
const subscriberMaxBufferedBytes = 4 << 20

// subscriberOutputFrameSize bounds how many bytes writeTo hands to a
// single Output write. Splitting a large flush into pieces this size
// limits how much of the stream one stalled write can tear (see
// protocol.WriteFrame) and lets a stall be detected within roughly one
// subscriberWriteTimeout of the consumer actually stopping, rather than
// after an entire multi-megabyte flush's deadline.
const subscriberOutputFrameSize = 32 << 10

// subscriberWriteTimeout bounds how long a single Output piece write to
// an attach connection may block. subscriberMaxBufferedBytes alone
// reclaims a subscriber that is merely slow (still draining, just more
// slowly than output is produced); this instead reclaims one whose
// consumer has stopped reading altogether, which would otherwise block
// the writer goroutine forever with no way to ever notice the subscriber
// has since been closed.
const subscriberWriteTimeout = 5 * time.Second

// subscriberFailureFlushTimeout bounds the one best-effort attempt to
// deliver a terminal Exit/Failure frame once a subscriber is closing --
// always a short, fixed-size frame, so it does not need
// subscriberWriteTimeout's more generous allowance for a large Output
// payload.
const subscriberFailureFlushTimeout = 2 * time.Second

// subscriberCloseReason records why a subscriber's output pump is ending,
// so writeTo knows which terminal frame, if any, to send before closing
// the connection.
type subscriberCloseReason uint8

const (
	subscriberOpen subscriberCloseReason = iota
	subscriberClosedByDetach
	subscriberClosedByProcessExit
	subscriberClosedByOverflow
	subscriberClosedByStall
)

// subscriber is one attach connection's PTY output pump. broadcast never
// touches the network connection directly: it only appends to buf, a
// buffer bounded by subscriberMaxBufferedBytes and drained by a dedicated
// writeTo goroutine. append is a fast, in-memory, lock-protected slice
// append, never an I/O call, so a slow or stalled consumer can never block
// PTY draining or any other subscriber.
type subscriber struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte
	inFlight int // bytes writeTo currently holds outside buf, not yet confirmed delivered
	reason   subscriberCloseReason
	detail   string // set for subscriberClosedByOverflow and subscriberClosedByStall
}

func newSubscriber() *subscriber {
	sub := &subscriber{}
	sub.cond = sync.NewCond(&sub.mu)
	return sub
}

// append adds chunk to the subscriber's pending output. If appending
// would push inFlight+buf past subscriberMaxBufferedBytes, chunk is
// rejected and the subscriber is instead marked closed with an overflow
// reason -- bytes already accepted are kept so writeTo can still flush
// them before reporting the failure. It reports whether this call is what
// closed the subscriber, so broadcast knows to drop it from the live set.
func (s *subscriber) append(chunk []byte) (closedNow bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason != subscriberOpen {
		return false
	}
	if s.inFlight+len(s.buf)+len(chunk) > subscriberMaxBufferedBytes {
		s.reason = subscriberClosedByOverflow
		s.detail = fmt.Sprintf("attach output buffer exceeded %d bytes; consumer was not keeping up", subscriberMaxBufferedBytes)
		s.cond.Broadcast()
		return true
	}
	s.buf = append(s.buf, chunk...)
	s.cond.Broadcast()
	return false
}

// close marks the subscriber closed for reason, unless it is already
// closed for a different one -- whichever reason wins the race is the one
// writeTo reports, and a later call never overwrites it.
func (s *subscriber) close(reason subscriberCloseReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason != subscriberOpen {
		return
	}
	s.reason = reason
	s.cond.Broadcast()
}

// writeTo drains the subscriber's buffered output to conn until it is
// closed, sends the terminal frame its close reason calls for, and always
// closes conn itself. Closing conn unconditionally -- not just on a
// failure path -- is what reclaims an attach whose consumer never sends
// anything and never hangs up on its own: it unblocks the attach loop's
// blocked protocol.Read(conn) regardless of client behavior.
func (s *subscriber) writeTo(conn net.Conn) {
	defer conn.Close()
	for {
		s.mu.Lock()
		for len(s.buf) == 0 && s.reason == subscriberOpen {
			s.cond.Wait()
		}
		pending := s.buf
		s.buf = nil
		s.inFlight = len(pending)
		reason, detail := s.reason, s.detail
		s.mu.Unlock()

		if !s.flush(conn, pending) {
			return // stalled or torn -- already reported (if it was safe to) and given up on above
		}
		switch reason {
		case subscriberOpen:
			continue
		case subscriberClosedByProcessExit:
			_ = conn.SetWriteDeadline(time.Now().Add(subscriberFailureFlushTimeout))
			_ = protocol.Write(conn, protocol.Exit, nil)
			return
		case subscriberClosedByOverflow, subscriberClosedByStall:
			s.sendFailure(conn, detail)
			return
		default: // subscriberClosedByDetach
			return
		}
	}
}

// flush writes pending to conn in subscriberOutputFrameSize pieces,
// reporting whether every piece was delivered. On failure it uses
// protocol.WriteFrame's torn result to decide what is still safe to do:
// a torn piece desynchronizes conn's framing, so nothing more is ever
// written to it, only closed (via writeTo's deferred conn.Close); a clean
// failure (the piece never touched the wire at all) leaves conn at a
// valid frame boundary, so flush marks the subscriber closed for a stall
// and makes one best-effort attempt to explain why before giving up --
// this is what lets a stalled consumer see an explicit reason instead of
// a bare EOF.
func (s *subscriber) flush(conn net.Conn, pending []byte) bool {
	for len(pending) > 0 {
		n := min(len(pending), subscriberOutputFrameSize)
		_ = conn.SetWriteDeadline(time.Now().Add(subscriberWriteTimeout))
		torn, err := protocol.WriteFrame(conn, protocol.Output, pending[:n])
		if err != nil {
			s.handleFlushFailure(conn, torn)
			return false
		}
		pending = pending[n:]
		s.mu.Lock()
		s.inFlight = len(pending)
		s.mu.Unlock()
	}
	return true
}

// handleFlushFailure decides and, when it is safe to, sends the terminal
// frame for an Output write that failed partway through a flush. The
// close reason it acts on may have been decided by this very failure
// (a still-open subscriber becomes subscriberClosedByStall), or it may
// already have been decided by a concurrent close()/append() call racing
// this same write (Detach, ProcessExit, Overflow) -- either way,
// handleFlushFailure re-reads s.reason itself rather than trusting a
// snapshot taken before the write, so a reason set by that race is always
// honored over turning the disconnect into a generic, undifferentiated
// stall. It never sends an empty Failure frame: Detach and ProcessExit
// each get the same terminal action a successful flush ending in that
// reason would have gotten (nothing, and Exit, respectively), never
// Failure.
func (s *subscriber) handleFlushFailure(conn net.Conn, torn bool) {
	s.mu.Lock()
	s.inFlight = 0
	if s.reason == subscriberOpen {
		s.reason = subscriberClosedByStall
		s.detail = "attach output write stalled: consumer stopped accepting output"
	}
	reason, detail := s.reason, s.detail
	s.mu.Unlock()

	if torn {
		return // framing is desynchronized: never write anything else to conn
	}
	switch reason {
	case subscriberClosedByOverflow, subscriberClosedByStall:
		s.sendFailure(conn, detail)
	case subscriberClosedByProcessExit:
		_ = conn.SetWriteDeadline(time.Now().Add(subscriberFailureFlushTimeout))
		_ = protocol.Write(conn, protocol.Exit, nil)
	case subscriberClosedByDetach:
		// no frame: the client already knows it is detaching
	}
}

// sendFailure makes one best-effort, short-deadline attempt to deliver a
// protocol.Failure frame explaining why the subscriber is being
// disconnected. Called only when the connection is known to still be at a
// clean frame boundary (see flush and writeTo).
func (s *subscriber) sendFailure(conn net.Conn, detail string) {
	_ = conn.SetWriteDeadline(time.Now().Add(subscriberFailureFlushTimeout))
	_ = protocol.Write(conn, protocol.Failure, []byte(detail))
}

type Server struct {
	Socket            string
	Store             *localstate.Store
	ResolveExecutable func(string) (string, error)
	mu                sync.RWMutex
	runs              map[string]*process
}

func (s *Server) Serve(ctx context.Context) error {
	if s.runs == nil {
		s.runs = map[string]*process{}
	}
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.Socket+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("supervisor is already running")
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if info, statErr := os.Lstat(s.Socket); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("refusing to replace non-socket supervisor path")
		}
		if err := os.Remove(s.Socket); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		return err
	}
	defer ln.Close()
	if err := os.Chmod(s.Socket, 0o600); err != nil {
		return err
	}
	if err := s.markStale(); err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go s.handle(c)
	}
}

func (s *Server) markStale() error {
	return s.Store.MarkAllRunningStale("supervisor restarted; PTY cannot be recovered")
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	kind, b, err := protocol.Read(c)
	if err != nil || kind != protocol.Request {
		return
	}
	var req Request
	if json.Unmarshal(b, &req) != nil {
		return
	}
	switch req.Action {
	case "start":
		s.start(c, req)
	case "attach":
		s.attach(c, req.RunID)
	case "stop":
		s.stop(c, req.RunID)
	case "ping":
		respond(c, s.handshake())
	case "preflight":
		s.preflight(c, req)
	default:
		respond(c, Response{Error: "unknown action"})
	}
}

func (s *Server) handshake() Response {
	executable, _ := os.Executable()
	cwd, _ := os.Getwd()
	identity, _ := processinfo.Observe(os.Getpid())
	return Response{
		OK: true, ProtocolVersion: ProtocolVersion, BuildVersion: BuildVersion,
		DaemonPID: os.Getpid(), DaemonStartTime: identity.StartTime, DaemonUID: identity.UID,
		DaemonExecutable: executable, DaemonParentPID: os.Getppid(), DaemonCWD: cwd, DaemonPATH: os.Getenv("PATH"),
	}
}

func (s *Server) resolve(provider string) (string, error) {
	name := provider
	if provider == "codex" {
		name = "codex"
	}
	if name == "" {
		return "", errors.New("provider executable is empty")
	}
	resolve := s.ResolveExecutable
	if resolve == nil {
		resolve = exec.LookPath
	}
	path, err := resolve(name)
	if err != nil {
		return "", fmt.Errorf("executable lookup for %s: %w", provider, err)
	}
	return path, nil
}

func (s *Server) preflight(c net.Conn, req Request) {
	path, err := s.resolve(req.Provider)
	if err != nil {
		respond(c, Response{Error: err.Error()})
		return
	}
	cmd := exec.Command(path, req.Args...)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		respond(c, Response{Error: fmt.Sprintf("direct exec %s: %v: %s", req.Provider, err, strings.TrimSpace(output.String())), ResolvedPath: path})
		return
	}
	respond(c, Response{OK: true, ResolvedPath: path, Output: output.String()})
}

func (s *Server) start(c net.Conn, req Request) {
	r := localstate.Run{ID: req.RunID, Provider: req.Provider, SessionID: req.SessionID, CWD: req.CWD, State: "starting", StartedAt: time.Now(), Baseline: append([]string(nil), req.Baseline...)}
	if err := s.Store.StartRun(r); err != nil {
		respond(c, Response{Error: err.Error()})
		return
	}
	path, err := s.resolve(req.Provider)
	if err != nil {
		_ = s.deleteRun(r.ID)
		respond(c, Response{Error: err.Error()})
		return
	}
	cmd := exec.Command(path, req.Args...)
	cmd.Dir = req.CWD
	overrides := make(map[string]string, len(req.Environment)+1)
	for key, value := range req.Environment {
		overrides[key] = value
	}
	overrides["CODEX_TUI_DISABLE_KEYBOARD_ENHANCEMENT"] = "1"
	cmd.Env = mergeEnvironment(os.Environ(), overrides)
	ptmx, tty, err := pty.Open()
	if err != nil {
		_ = s.deleteRun(r.ID)
		respond(c, Response{Error: "PTY setup: " + err.Error()})
		return
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		_ = tty.Close()
		_ = ptmx.Close()
		_ = s.deleteRun(r.ID)
		respond(c, Response{Error: "PTY process spawn: " + err.Error(), ResolvedPath: path})
		return
	}
	_ = tty.Close()
	r.State = "running"
	r.PID = cmd.Process.Pid
	if identity, observeErr := processinfo.Observe(r.PID); observeErr == nil {
		r.StartTime, r.UID = identity.StartTime, identity.UID
	} else {
		r.Error = "process identity unavailable: " + observeErr.Error()
	}
	p := &process{run: r, cmd: cmd, ptmx: ptmx, subscribers: map[*subscriber]struct{}{}, done: make(chan struct{})}
	p.editorRedraw = newExternalEditorRedrawDetector(req.Provider, cmd.Process.Pid)
	s.mu.Lock()
	s.runs[r.ID] = p
	s.mu.Unlock()
	_ = s.saveRun(r)
	go s.drain(p)
	respond(c, Response{OK: true, Run: &r})
}
func (s *Server) drain(p *process) {
	runID := p.run.ID
	buf := make([]byte, 32<<10)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			redraw := p.editorRedraw != nil && p.editorRedraw.Observe(chunk)
			p.broadcast(chunk)
			if redraw {
				forcePTYRedraw(p)
			}
		}
		if err != nil {
			break
		}
	}
	_ = p.cmd.Wait()
	_ = p.ptmx.Close()
	p.finishSubscribers()
	stopped, _ := s.Store.MarkRunStopped(runID)
	p.mu.Lock()
	p.run = stopped
	p.mu.Unlock()
	s.mu.Lock()
	delete(s.runs, runID)
	s.mu.Unlock()
}
func (s *Server) attach(c net.Conn, id string) {
	s.mu.RLock()
	p := s.runs[id]
	s.mu.RUnlock()
	if p == nil {
		respond(c, Response{Error: "managed run is not live"})
		return
	}
	p.mu.Lock()
	run := p.run
	p.mu.Unlock()
	respond(c, Response{OK: true, Run: &run})
	sub := newSubscriber()
	if !p.addSubscriber(sub) {
		_ = protocol.Write(c, protocol.Exit, nil)
		return
	}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		sub.writeTo(c)
	}()
	// removeSubscriber runs first (LIFO) so a return from any branch below
	// (read error, Detach) marks sub closed and wakes writeTo; only then
	// do we wait for it, so this can never hang on a subscriber that was
	// never told to stop. writeTo closing c also covers the reverse order
	// -- a writer-initiated close (process exit, overflow) -- by
	// unblocking the protocol.Read below regardless of client behavior.
	defer func() { <-writerDone }()
	defer p.removeSubscriber(sub)
	for {
		kind, b, err := protocol.Read(c)
		if err != nil {
			return
		}
		switch kind {
		case protocol.Input:
			_, _ = p.ptmx.Write(b)
		case protocol.Resize:
			var sz protocol.TerminalSize
			if json.Unmarshal(b, &sz) == nil {
				syncPTYSize(p, sz)
			}
		case protocol.Detach:
			return
		}
	}
}

func (p *process) broadcast(chunk []byte) {
	p.mu.Lock()
	for sub := range p.subscribers {
		if sub.append(chunk) {
			delete(p.subscribers, sub)
		}
	}
	p.mu.Unlock()
}

func (p *process) addSubscriber(sub *subscriber) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return false
	default:
		p.subscribers[sub] = struct{}{}
		return true
	}
}

func (p *process) removeSubscriber(sub *subscriber) {
	p.mu.Lock()
	delete(p.subscribers, sub)
	p.mu.Unlock()
	sub.close(subscriberClosedByDetach)
}

func (p *process) finishSubscribers() {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.done)
	for sub := range p.subscribers {
		sub.close(subscriberClosedByProcessExit)
	}
	p.subscribers = map[*subscriber]struct{}{}
}

// syncPTYSize applies the client's terminal size to the managed PTY and, on
// reattach (Redraw requested, size unchanged from before), forces the child
// to repaint.
//
// A manually raised SIGWINCH with no underlying size change (tried first)
// was verified against the installed Codex CLI to NOT be sufficient: Codex's
// resize handling re-reads the PTY size on the signal and skips repainting
// when it finds no difference from what it already has cached, so the
// attaching client is left staring at whatever was on screen before it
// connected (nothing, since a new subscriber gets no scrollback replay --
// see attach() below) until the child happens to repaint on its own. A real
// TIOCSWINSZ size change, by contrast, makes the kernel deliver SIGWINCH on
// its own, and Codex's own verified behavior is to always repaint when the
// size genuinely changes. So on a same-size reattach this bounces the PTY to
// a harmless alternate size and immediately back: two genuine size changes,
// landing back at the real terminal size, each a real kernel-delivered
// SIGWINCH rather than a synthetic one Codex can decide to ignore.
//
// The two Setsize calls are separated by a short sleep. SIGWINCH is a
// standard (non-realtime) signal: POSIX does not queue repeat occurrences,
// so if both size changes land before the child is scheduled to handle the
// first signal, the kernel coalesces them into one delivery and the child
// observes only the final size -- identical to what it already had, so it
// correctly (from its own honest, verified redraw-on-real-change behavior)
// does not repaint. This was verified directly: without the delay, a test
// child modeling Codex's real resize-diffing behavior never saw the bounce.
func syncPTYSize(p *process, size protocol.TerminalSize) {
	if size.Rows == 0 || size.Cols == 0 {
		return
	}
	p.resizeMu.Lock()
	defer p.resizeMu.Unlock()

	current, err := pty.GetsizeFull(p.ptmx)
	unchanged := err == nil && current.Rows == size.Rows && current.Cols == size.Cols
	if unchanged && size.Redraw {
		forcePTYRedrawLocked(p, current)
		return
	}
	_ = pty.Setsize(p.ptmx, &pty.Winsize{Rows: size.Rows, Cols: size.Cols})
}

func forcePTYRedraw(p *process) {
	p.resizeMu.Lock()
	defer p.resizeMu.Unlock()

	current, err := pty.GetsizeFull(p.ptmx)
	if err != nil || current.Rows == 0 || current.Cols == 0 {
		return
	}
	forcePTYRedrawLocked(p, current)
}

func forcePTYRedrawLocked(p *process, size *pty.Winsize) {
	bounce := size.Rows - 1
	if bounce == 0 {
		bounce = size.Rows + 1
	}
	_ = pty.Setsize(p.ptmx, &pty.Winsize{Rows: bounce, Cols: size.Cols})
	time.Sleep(50 * time.Millisecond)
	_ = pty.Setsize(p.ptmx, size)
}

// externalEditorRedrawDetector recognizes the terminal lifecycle produced by
// Codex around a full-screen external editor. These are terminal protocol
// modes, not editor names or UI text: Codex disables bracketed paste before
// yielding the terminal, the editor enters and leaves the alternate screen,
// and Codex enables bracketed paste after regaining control. The detector
// identifies the new direct child at alternate-screen entry and waits for that
// same process identity to disappear. This avoids confusing a persistent MCP
// child with the editor and distinguishes an editor launch failure from a
// successful handoff. Codex waits for the editor child before resuming, so the
// final mode enable establishes that the child has exited.
//
// Waiting for the editor's alternate-screen leave before accepting Codex's
// resume avoids resizing the PTY while the editor is active. Observe is called
// only by drain, so its state needs no synchronization and ends with the
// managed process instead of requiring a polling goroutine.
type externalEditorRedrawDetector struct {
	state              externalEditorRedrawState
	pending            []byte
	knownChildren      map[processinfo.Identity]struct{}
	editor             processinfo.Identity
	listDirectChildren func() []processinfo.Identity
	refreshChildren    bool
}

func newExternalEditorRedrawDetector(provider string, pid int) *externalEditorRedrawDetector {
	if provider != "codex" {
		return nil
	}
	detector := &externalEditorRedrawDetector{
		listDirectChildren: func() []processinfo.Identity { return directChildren(pid) },
	}
	detector.refreshKnownChildren()
	return detector
}

type externalEditorRedrawState uint8

const (
	waitingForCodexSuspend externalEditorRedrawState = iota
	waitingForEditorScreen
	waitingForEditorReturn
	waitingForCodexResume
)

var terminalLifecycleSequences = []struct {
	sequence []byte
	event    byte
}{
	{[]byte("\x1b[?2004l"), 'd'},
	{[]byte("\x1b[?2004h"), 'e'},
	{[]byte("\x1b[?1049h"), 'i'},
	{[]byte("\x1b[?1049l"), 'o'},
}

func (d *externalEditorRedrawDetector) Observe(chunk []byte) bool {
	data := append(d.pending, chunk...)
	redraw := false
	for len(data) > 0 {
		index, event, length := nextTerminalLifecycleEvent(data)
		if index < 0 {
			break
		}
		if d.observeEvent(event) {
			redraw = true
		}
		data = data[index+length:]
	}
	const longestSequence = len("\x1b[?2004l")
	if len(data) >= longestSequence {
		data = data[len(data)-longestSequence+1:]
	}
	d.pending = append(d.pending[:0], data...)
	if d.refreshChildren {
		d.refreshKnownChildren()
		d.refreshChildren = false
	}
	return redraw
}

func nextTerminalLifecycleEvent(data []byte) (int, byte, int) {
	index, event, length := -1, byte(0), 0
	for _, candidate := range terminalLifecycleSequences {
		found := bytes.Index(data, candidate.sequence)
		if found >= 0 && (index < 0 || found < index) {
			index, event, length = found, candidate.event, len(candidate.sequence)
		}
	}
	return index, event, length
}

func (d *externalEditorRedrawDetector) observeEvent(event byte) bool {
	switch d.state {
	case waitingForCodexSuspend:
		if event == 'd' {
			d.state = waitingForEditorScreen
		}
	case waitingForEditorScreen:
		if event == 'i' {
			if editor, ok := d.newDirectChild(); ok {
				d.editor = editor
				d.state = waitingForEditorReturn
			} else {
				d.state = waitingForCodexSuspend
				d.refreshChildren = true
			}
		} else if event == 'e' {
			d.state = waitingForCodexSuspend
			d.refreshChildren = true
		}
	case waitingForEditorReturn:
		if event == 'o' {
			d.state = waitingForCodexResume
		}
	case waitingForCodexResume:
		if event == 'e' {
			d.state = waitingForCodexSuspend
			d.refreshChildren = true
			return !d.editorIsRunning()
		}
	}
	return false
}

func (d *externalEditorRedrawDetector) refreshKnownChildren() {
	d.knownChildren = make(map[processinfo.Identity]struct{})
	if d.listDirectChildren == nil {
		return
	}
	for _, child := range d.listDirectChildren() {
		d.knownChildren[child] = struct{}{}
	}
}

func (d *externalEditorRedrawDetector) newDirectChild() (processinfo.Identity, bool) {
	if d.listDirectChildren == nil {
		return processinfo.Identity{}, false
	}
	var found processinfo.Identity
	for _, child := range d.listDirectChildren() {
		if _, known := d.knownChildren[child]; known {
			continue
		}
		if !found.Valid() || child.StartTime > found.StartTime {
			found = child
		}
	}
	return found, found.Valid()
}

func (d *externalEditorRedrawDetector) editorIsRunning() bool {
	if d.listDirectChildren == nil {
		return false
	}
	for _, child := range d.listDirectChildren() {
		if child == d.editor {
			return true
		}
	}
	return false
}
func (s *Server) stop(c net.Conn, id string) {
	s.mu.RLock()
	p := s.runs[id]
	s.mu.RUnlock()
	if p == nil {
		respond(c, Response{Error: "refusing to stop a run not owned by this supervisor"})
		return
	}
	p.mu.Lock()
	run := p.run
	p.mu.Unlock()
	if err := processinfo.Match(processinfo.Identity{PID: run.PID, StartTime: run.StartTime, UID: run.UID}); err != nil {
		respond(c, Response{Error: "refusing to signal changed process identity: " + err.Error()})
		return
	}
	if err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM); err != nil {
		respond(c, Response{Error: err.Error()})
		return
	}
	select {
	case <-p.done:
		respond(c, Response{OK: true})
	case <-time.After(5 * time.Second):
		respond(c, Response{Error: "SIGTERM timed out; process was not force-killed"})
	}
}
func (s *Server) saveRun(r localstate.Run) error { return s.Store.SaveRun(r) }
func (s *Server) deleteRun(id string) error      { return s.Store.DeleteRun(id) }
func respond(w io.Writer, r Response) {
	b, _ := json.Marshal(r)
	_ = protocol.Write(w, protocol.Response, b)
}

type Client struct {
	Socket     string
	DaemonPath string
	StatePath  string
}

func (c Client) Ensure(ctx context.Context) error {
	if conn, err := net.DialTimeout("unix", c.Socket, 100*time.Millisecond); err == nil {
		peer, peerErr := peerIdentity(conn)
		response, pingErr := call(conn, Request{Action: "ping"})
		_ = conn.Close()
		if pingErr == nil && compatible(response) {
			return nil
		}
		if pingErr != nil {
			return fmt.Errorf("supervisor handshake: %w", pingErr)
		}
		var restartErr error
		if response.DaemonPID == 0 && response.DaemonStartTime == 0 && response.BuildVersion == "" {
			if peerErr != nil {
				restartErr = fmt.Errorf("legacy daemon peer identity: %w", peerErr)
			} else {
				restartErr = c.restartLegacyOwned(ctx, peer)
			}
		} else {
			restartErr = c.restartOwned(ctx, response)
		}
		if restartErr != nil {
			return fmt.Errorf("incompatible supervisor protocol/build: %w", restartErr)
		}
	}
	return c.start(ctx)
}

func (c Client) start(ctx context.Context) error {
	cmd := exec.Command(c.DaemonPath, "daemon", "--state", c.StatePath, "--socket", c.Socket)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("unix", c.Socket, 100*time.Millisecond); err == nil {
			_ = conn.Close()
			response, pingErr := c.Call(ctx, Request{Action: "ping"})
			if pingErr != nil {
				return fmt.Errorf("new supervisor handshake: %w", pingErr)
			}
			if !compatible(response) {
				return fmt.Errorf("new supervisor has incompatible protocol/build %d/%q", response.ProtocolVersion, response.BuildVersion)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return errors.New("supervisor did not start")
}

func compatible(response Response) bool {
	return response.ProtocolVersion == ProtocolVersion && response.BuildVersion == BuildVersion
}

func (c Client) restartOwned(ctx context.Context, response Response) error {
	if response.DaemonPID <= 0 || response.DaemonStartTime == 0 || response.DaemonUID != uint32(os.Getuid()) {
		return errors.New("existing daemon ownership is not verifiable")
	}
	if !sameExecutable(response.DaemonExecutable, c.DaemonPath) {
		return errors.New("existing daemon executable does not match this agentsctl binary")
	}
	// An incompatible daemon still owns the PTY and process of any
	// managed run it is running (see the DesignDoc's Codex supervisor
	// Lifetime section): killing it here would lose that run exactly the
	// way a supervisor crash does, with no chance for the run to finish
	// or be attached to again. Refuse rather than silently discard it --
	// mirrors restartLegacyOwned's identical check for the pre-versioning
	// daemon case.
	runs, err := localstate.New(c.StatePath).Runs()
	if err != nil {
		return fmt.Errorf("inspect existing daemon state: %w", err)
	}
	for _, run := range runs {
		if run.State == "running" || run.State == "starting" {
			return errors.New("existing daemon still owns an active managed run; stop it or wait for it to finish, then retry")
		}
	}
	if err := processinfo.Match(processinfo.Identity{PID: response.DaemonPID, StartTime: response.DaemonStartTime, UID: response.DaemonUID}); err != nil {
		return fmt.Errorf("existing daemon identity changed: %w", err)
	}
	if err := syscall.Kill(response.DaemonPID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop owned daemon: %w", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("unix", c.Socket, 50*time.Millisecond)
		if dialErr != nil {
			return nil
		}
		_ = conn.Close() // still alive: this poll's own connection must not leak and sit unread on the daemon's side
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return errors.New("owned daemon did not stop")
}

func (c Client) restartLegacyOwned(ctx context.Context, identity processinfo.Identity) error {
	if !identity.Valid() || identity.UID != uint32(os.Getuid()) {
		return errors.New("legacy daemon peer ownership is not verifiable")
	}
	if err := privateEndpoint(c.Socket, identity.UID); err != nil {
		return fmt.Errorf("legacy daemon endpoint: %w", err)
	}
	name, err := processinfo.Name(identity.PID)
	if err != nil {
		return fmt.Errorf("legacy daemon process name: %w", err)
	}
	if !processNameMatches(name, filepath.Base(c.DaemonPath)) {
		return fmt.Errorf("legacy daemon process %q does not match %q", name, filepath.Base(c.DaemonPath))
	}
	runs, err := localstate.New(c.StatePath).Runs()
	if err != nil {
		return fmt.Errorf("inspect legacy daemon state: %w", err)
	}
	for _, run := range runs {
		if run.State == "running" || run.State == "starting" {
			return errors.New("legacy daemon still owns an active run; refusing automatic restart")
		}
	}
	if err := processinfo.Match(identity); err != nil {
		return fmt.Errorf("legacy daemon identity changed: %w", err)
	}
	if err := syscall.Kill(identity.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop owned legacy daemon: %w", err)
	}
	return c.waitStopped(ctx, "owned legacy daemon")
}

func (c Client) waitStopped(ctx context.Context, label string) error {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("unix", c.Socket, 50*time.Millisecond)
		if dialErr != nil {
			return nil
		}
		_ = conn.Close() // still alive: this poll's own connection must not leak and sit unread on the daemon's side
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s did not stop", label)
}

func privateEndpoint(socket string, uid uint32) error {
	directory, err := os.Stat(filepath.Dir(socket))
	if err != nil {
		return err
	}
	if !directory.IsDir() || directory.Mode().Perm()&0o077 != 0 || fileUID(directory) != uid {
		return errors.New("state directory is not private to the daemon user")
	}
	endpoint, err := os.Lstat(socket)
	if err != nil {
		return err
	}
	if endpoint.Mode()&os.ModeSocket == 0 || endpoint.Mode().Perm()&0o077 != 0 || fileUID(endpoint) != uid {
		return errors.New("socket is not private to the daemon user")
	}
	return nil
}

func fileUID(info os.FileInfo) uint32 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Uid
	}
	return ^uint32(0)
}

func processNameMatches(actual, expected string) bool {
	if actual == expected {
		return true
	}
	// Linux and Darwin kernel process names are truncated to a small fixed field.
	return len(actual) >= 15 && strings.HasPrefix(expected, actual)
}

func sameExecutable(left, right string) bool {
	leftPath, leftErr := filepath.EvalSymlinks(left)
	rightPath, rightErr := filepath.EvalSymlinks(right)
	if leftErr == nil && rightErr == nil {
		leftInfo, leftStatErr := os.Stat(leftPath)
		rightInfo, rightStatErr := os.Stat(rightPath)
		if leftStatErr == nil && rightStatErr == nil {
			return os.SameFile(leftInfo, rightInfo)
		}
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func (c Client) Preflight(ctx context.Context, provider string, args ...string) (Response, error) {
	return c.Call(ctx, Request{Action: "preflight", Provider: provider, Args: append([]string(nil), args...)})
}
func (c Client) Call(ctx context.Context, req Request) (Response, error) {
	conn, err := net.Dial("unix", c.Socket)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	return call(conn, req)
}

func call(conn net.Conn, req Request) (Response, error) {
	b, _ := json.Marshal(req)
	if err := protocol.Write(conn, protocol.Request, b); err != nil {
		return Response{}, err
	}
	kind, b, err := protocol.Read(conn)
	if err != nil || kind != protocol.Response {
		return Response{}, fmt.Errorf("invalid supervisor response: %w", err)
	}
	var res Response
	if err := json.Unmarshal(b, &res); err != nil {
		return res, err
	}
	if !res.OK {
		return res, errors.New(res.Error)
	}
	return res, nil
}

func mergeEnvironment(environment []string, overrides map[string]string) []string {
	result := make([]string, 0, len(environment)+len(overrides))
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, overridden := overrides[key]; overridden {
				continue
			}
		}
		result = append(result, entry)
	}
	for key, value := range overrides {
		result = append(result, key+"="+value)
	}
	return result
}
