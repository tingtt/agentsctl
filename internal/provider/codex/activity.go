package codex

import "github.com/tingtt/agentsctl/internal/session"

// Native ThreadStatus.Type values (Codex app-server protocol v2).
const (
	statusNotLoaded   = "notLoaded"
	statusIdle        = "idle"
	statusActive      = "active"
	statusSystemError = "systemError"
)

// Known ThreadStatus.ActiveFlags values that mean the running turn waits on
// the user. Any other flag leaves an active thread Working.
const (
	flagWaitingOnApproval  = "waitingOnApproval"
	flagWaitingOnUserInput = "waitingOnUserInput"
)

// nativeActivity maps a loaded thread's native status onto the common
// Activity. It never yields ActivityCompleted: a finished turn is a
// transient event, the persistent state it leaves behind is idle. notLoaded
// is outside this mapping (it is Unknown here) because a status alone
// cannot tell a dormant thread from one running outside the queried
// app-server -- see observeThread.
func nativeActivity(status ThreadStatus) session.Activity {
	switch status.Type {
	case statusActive:
		for _, flag := range status.ActiveFlags {
			if flag == flagWaitingOnApproval || flag == flagWaitingOnUserInput {
				return session.ActivityNeedsInput
			}
		}
		return session.ActivityWorking
	case statusIdle:
		return session.ActivityIdle
	case statusSystemError:
		return session.ActivityFailed
	default:
		return session.ActivityUnknown
	}
}

// observation is the display-side Activity/Runtime of one catalog thread.
// Of a row's actions it only informs advisory Open availability (see
// Provider.sessionRows); Open itself re-checks against the daemon.
type observation struct {
	Activity session.Activity
	Runtime  session.Runtime
}

// unobserved is the observation of a thread whose runtime cannot currently
// be observed at all (the shared app-server connection is down).
var unobserved = observation{Activity: session.ActivityUnknown, Runtime: session.RuntimeUnknown}

// observeThread resolves a native status into an observation. A status
// other than notLoaded comes from the app-server that has the thread
// loaded, so it is authoritative on its own and the writer lock is never
// consulted: that app-server holds the lock itself. Only for notLoaded does
// writerFree decide between a dormant thread and a runtime outside the
// app-server.
func observeThread(status ThreadStatus, writerFree func() bool) observation {
	if status.Type != statusNotLoaded {
		return observation{Activity: nativeActivity(status), Runtime: session.RuntimeDetached}
	}
	if writerFree() {
		return observation{Activity: session.ActivityIdle, Runtime: session.RuntimeNone}
	}
	return observation{Activity: session.ActivityUnknown, Runtime: session.RuntimeExternal}
}
