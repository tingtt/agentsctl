package work

import "time"

// CommentID is a GitHub REST numeric issue-comment identifier.
type CommentID int64

// User identifies a GitHub comment author or editor.
type User struct {
	Login string
	Type  string
}

// Comment is the GitHub-independent issue comment representation consumed by
// protocol parsing and history reconstruction.
type Comment struct {
	ID                CommentID
	NodeID            string
	Body              string
	HTMLURL           string
	Author            User
	AuthorAssociation string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Trust             Trust
}

// Prompt is protocol v1 Prompt metadata.
type Prompt struct {
	Continues *CommentID
}

// State is a durable Status state.
type State string

const (
	StateStarting    State = "starting"
	StateRunning     State = "running"
	StateCompleted   State = "completed"
	StateFailed      State = "failed"
	StateInterrupted State = "interrupted"
	StateStopped     State = "stopped"
	StateAbandoned   State = "abandoned"
	StateSuperseded  State = "superseded"
)

// Active reports whether provider execution may still be in progress.
func (s State) Active() bool { return s == StateStarting || s == StateRunning }

// Status is protocol v1 Status metadata.
type Status struct {
	Prompt       CommentID
	Dispatch     string
	Host         string
	Provider     string
	Session      string
	Turn         string
	State        State
	PromptDigest string
}

// ProtocolKind classifies an issue comment without applying author trust.
type ProtocolKind uint8

const (
	Discussion ProtocolKind = iota
	ValidPrompt
	ValidStatus
	MalformedProtocol
	UnsupportedProtocol
	UnknownProtocol
)

// ParsedComment is the syntax-only result of parsing one comment body.
type ParsedComment struct {
	Kind        ProtocolKind
	VisibleBody string
	Prompt      *Prompt
	Status      *Status
	Type        string
	Version     int
	Fields      map[string]string
	Err         error
}

// TrustReason explains why a comment is or is not trusted for automatic use.
type TrustReason string

const (
	TrustUnchecked          TrustReason = "unchecked"
	TrustEstablished        TrustReason = "trusted"
	TrustAuthorNotUser      TrustReason = "author-not-user"
	TrustLoginNotAllowed    TrustReason = "login-not-allowed"
	TrustPermissionReadOnly TrustReason = "permission-below-write"
	TrustPermissionUnknown  TrustReason = "permission-unknown"
	TrustEditorUnknown      TrustReason = "editor-unknown"
	TrustEditorNotUser      TrustReason = "editor-not-user"
	TrustEditorLoginDenied  TrustReason = "editor-login-not-allowed"
	TrustEditorReadOnly     TrustReason = "editor-permission-below-write"
)

// Trust is separate from protocol syntax and retains a diagnostic reason.
type Trust struct {
	Trusted bool
	Reason  TrustReason
	Detail  string
}

// WarningKind identifies non-authoritative history information.
type WarningKind string

const (
	WarningMalformed             WarningKind = "malformed-protocol"
	WarningUnsupported           WarningKind = "unsupported-protocol"
	WarningUnknownType           WarningKind = "unknown-protocol-type"
	WarningUntrusted             WarningKind = "untrusted-protocol"
	WarningOrphanStatus          WarningKind = "orphan-status"
	WarningInvalidContinuation   WarningKind = "invalid-continuation"
	WarningDuplicateContinuation WarningKind = "duplicate-continuation"
)

// Warning preserves a protocol or relation problem for later diagnostics.
type Warning struct {
	CommentID CommentID
	Kind      WarningKind
	Detail    string
}

// PromptHistory is the reconstructed state for one Prompt.
type PromptHistory struct {
	Comment               Comment
	Prompt                Prompt
	Statuses              []StatusHistory
	CurrentStatus         *StatusHistory
	Dispatched            bool
	Active                bool
	Pending               bool
	BlockedByUnsupported  bool
	ContinuationValid     bool
	ContinuationTrusted   bool
	CanonicalContinuation bool
	Eligible              bool
	AutoTrusted           bool
}

// StatusHistory is one syntax-valid Status and its authoritative disposition.
type StatusHistory struct {
	Comment       Comment
	Status        Status
	Authoritative bool
	Orphan        bool
}

// UnsupportedStatus records a future Status version whose target can still
// be identified, preventing accidental redispatch.
type UnsupportedStatus struct {
	Comment Comment
	Prompt  CommentID
	Version int
}

// History is a deterministic reconstruction of an Issue's current comments.
type History struct {
	Prompts             []PromptHistory
	Statuses            []StatusHistory
	UnsupportedStatuses []UnsupportedStatus
	Warnings            []Warning
}
