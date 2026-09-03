package session

import "time"

type ProviderID string

const (
	ProviderClaude ProviderID = "claude"
	ProviderCodex  ProviderID = "codex"
)

type Key struct {
	Provider ProviderID `json:"provider"`
	ID       string     `json:"id"`
}

func (k Key) String() string { return string(k.Provider) + ":" + k.ID }

type Activity string

const (
	ActivityStarting     Activity = "starting"
	ActivityWorking      Activity = "working"
	ActivityNeedsInput   Activity = "needsInput"
	ActivityWaitingQuota Activity = "waitingForQuota"
	ActivityIdle         Activity = "idle"
	ActivityCompleted    Activity = "completed"
	ActivityFailed       Activity = "failed"
	ActivityUnknown      Activity = "unknown"
)

type Runtime string

const (
	RuntimeAttached Runtime = "attached"
	RuntimeDetached Runtime = "detached"
	RuntimeExternal Runtime = "external"
	RuntimeStopped  Runtime = "stopped"
	RuntimeNone     Runtime = "none"
	RuntimeUnknown  Runtime = "unknown"
)

type Capabilities struct {
	Attach    bool   `json:"attach"`
	Stop      bool   `json:"stop"`
	Rename    bool   `json:"rename"`
	Archive   bool   `json:"archive"`
	Unarchive bool   `json:"unarchive"`
	Respawn   bool   `json:"respawn"`
	Reason    string `json:"reason,omitempty"`
}

type Session struct {
	Key          Key          `json:"key"`
	Name         string       `json:"name"`
	Summary      string       `json:"summary"`
	CWD          string       `json:"cwd"`
	UpdatedAt    time.Time    `json:"updatedAt"`
	Activity     Activity     `json:"activity"`
	Runtime      Runtime      `json:"runtime"`
	Archived     bool         `json:"archived"`
	Capabilities Capabilities `json:"capabilities"`
	RunID        string       `json:"runId,omitempty"`
}

func (s Session) DisplayName() string {
	if s.Name != "" {
		return s.Name
	}
	if s.Summary != "" {
		return s.Summary
	}
	return s.Key.ID
}

func CapabilitiesFor(s Session) Capabilities {
	c := s.Capabilities
	active := s.Activity == ActivityWorking || s.Activity == ActivityNeedsInput || s.Activity == ActivityStarting
	runtimeActive := s.Runtime == RuntimeAttached || s.Runtime == RuntimeDetached || s.Runtime == RuntimeExternal
	if active || runtimeActive {
		c.Archive = false
		if c.Reason == "" {
			c.Reason = "stop the session before archiving"
		}
	}
	if s.Archived {
		c.Attach, c.Stop, c.Rename, c.Archive, c.Respawn = false, false, false, false, false
		c.Unarchive = true
	}
	return c
}
