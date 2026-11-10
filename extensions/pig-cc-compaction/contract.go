// Package pigcc reproduces Claude Code's compaction on PiG: the provider request
// a session last sent is resent verbatim with one summary instruction appended,
// so the provider's prompt cache still hits, and the nine-section <summary> it
// answers with becomes the new context.
//
// Everything in this file is plain data and interfaces: no pig host, no sdk
// import, so every layer around the host stays unit-testable.
package pigcc

import (
	"context"
	"encoding/json"
	"time"
)

// Capture is the provider payload a session last sent, kept verbatim.
type Capture struct {
	Payload  json.RawMessage
	ModelID  string
	Provider string
	At       time.Time
}

// Breaker is the per-session state the JS policy reads before taking over.
type Breaker struct {
	Fails    int  `json:"fails"`
	Thrash   int  `json:"thrash"`
	Disabled bool `json:"disabled"`
}

// FileOps mirrors Pi's preparation.fileOps.
type FileOps struct {
	Read    []string `json:"read"`
	Written []string `json:"written"`
	Edited  []string `json:"edited"`
}

// Settings mirrors Pi's CompactionSettings.
type Settings struct {
	Enabled          bool `json:"enabled"`
	ReserveTokens    int  `json:"reserveTokens"`
	KeepRecentTokens int  `json:"keepRecentTokens"`
}

// Preparation mirrors Pi's CompactionPreparation.
type Preparation struct {
	FirstKeptEntryID string `json:"firstKeptEntryId"`
	TokensBefore     int    `json:"tokensBefore"`
	PreviousSummary  string `json:"previousSummary,omitempty"`
	IsSplitTurn      bool   `json:"isSplitTurn"`
	// TurnPrefixMessages is the start of the turn the cut point falls into:
	// the messages before it are summarized, these are kept verbatim, and Pi
	// asks for a checkpoint of them so the retained range still starts with
	// context. Empty unless IsSplitTurn.
	TurnPrefixMessages []json.RawMessage `json:"turnPrefixMessages"`
	FileOps            FileOps           `json:"fileOps"`
	Settings           Settings          `json:"settings"`
}

// CompactEvent is the session_before_compact payload, decoded.
type CompactEvent struct {
	Reason             string      `json:"reason"`
	CustomInstructions string      `json:"customInstructions,omitempty"`
	WillRetry          bool        `json:"willRetry"`
	Preparation        Preparation `json:"preparation"`
}

// TraceRecord is one line of the trace log. cacheRead above zero is the only
// evidence that the resent prefix actually hit the provider's cache.
type TraceRecord struct {
	TS              time.Time `json:"ts"`
	SessionID       string    `json:"sessionId"`
	Outcome         string    `json:"outcome"`
	Detail          string    `json:"detail,omitempty"`
	InputTokens     int       `json:"inputTokens"`
	CacheReadTokens int       `json:"cacheReadTokens"`
	OutputTokens    int       `json:"outputTokens"`
	DurationMs      int64     `json:"durationMs"`
}

// Trace outcomes.
const (
	OutcomeCompacted = "compacted"
	OutcomeFallback  = "fallback"
	OutcomeFailed    = "failed"
	// OutcomeRetry records a reply the policy rejected while another attempt was
	// still allowed. It is not a compaction outcome and never reaches the user:
	// it exists so the reason a reply was refused survives a retry that worked.
	OutcomeRetry = "retry"
	// OutcomeEvicted is not a compaction: it records that the capture table
	// dropped a session to stay within maxSessions, which costs that session one
	// cold compaction and resets its breaker. It never reaches the user.
	OutcomeEvicted = "evicted"
)

// CaptureIn is the capture a session has, as the JS side sees it.
type CaptureIn struct {
	Payload  json.RawMessage `json:"payload"`
	ModelID  string          `json:"modelId"`
	Provider string          `json:"provider"`
	AgeMs    int64           `json:"ageMs"`
}

// PrepIn is the part of the preparation the JS policy reads.
type PrepIn struct {
	FirstKeptEntryID string `json:"firstKeptEntryId"`
	TokensBefore     int    `json:"tokensBefore"`
	PreviousSummary  string `json:"previousSummary,omitempty"`
}

// PlanInput crosses into JS for the planCompact hook.
type PlanInput struct {
	Reason             string     `json:"reason"`
	Attempt            int        `json:"attempt"`
	CustomInstructions string     `json:"customInstructions,omitempty"`
	Capture            *CaptureIn `json:"capture"`
	Breaker            Breaker    `json:"breaker"`
	Preparation        PrepIn     `json:"preparation"`
}

// PlanOutput comes back from the planCompact hook. A takeover carries the
// replacement provider payload and the host-level request to stream.
type PlanOutput struct {
	Action      string          `json:"action"`
	Why         string          `json:"why,omitempty"`
	Request     json.RawMessage `json:"request,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	PromptChars int             `json:"promptChars,omitempty"`
}

// ParseInput crosses into JS for the parseSummary hook.
type ParseInput struct {
	Text         string `json:"text"`
	StopReason   string `json:"stopReason,omitempty"`
	OutputTokens int    `json:"outputTokens,omitempty"`
	Attempt      int    `json:"attempt"`
}

// ParseOutput comes back from the parseSummary hook: a summary, a stricter
// re-ask, or nothing usable.
type ParseOutput struct {
	Summary string `json:"summary,omitempty"`
	Retry   bool   `json:"retry,omitempty"`
	None    bool   `json:"none,omitempty"`
	Why     string `json:"why,omitempty"`
}

// Plugin is the moejs core: one hook per phase, JSON in and JSON out.
type Plugin interface {
	Plan(ctx context.Context, in PlanInput) (PlanOutput, error)
	Parse(ctx context.Context, in ParseInput) (ParseOutput, error)
}

// State keeps each session's captured payload and breaker counters.
type State interface {
	Put(sessionID, modelID, provider string, payload json.RawMessage)
	Get(sessionID string) (Capture, bool)
	Forget(sessionID string)
	Breaker(sessionID string) Breaker
	NoteSuccess(sessionID string)
	NoteFailure(sessionID string)
}

// Sink is the trace log.
type Sink interface {
	Append(rec TraceRecord) error
}
