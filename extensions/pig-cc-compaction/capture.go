package pigcc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Capacity and policy constants for the per-session capture table.
const (
	// maxSessions bounds memory in the extension process; the least recently
	// used session is evicted when a new one would exceed the bound.
	maxSessions = 32
	// failLimit consecutive failures trip the breaker.
	failLimit = 3
	// thrashLimit successes inside thrashWindow trip the breaker.
	thrashLimit = 3
	// thrashWindow is how close two successful compactions must be to count as
	// one thrash burst.
	thrashWindow = 30 * time.Second
)

// now is time.Now; tests replace it so they can cross the thrash window
// without sleeping.
var now = time.Now

// sessionEntry is the mutable per-session record. Its capture, breaker and
// timestamps all move together under MemState's lock.
type sessionEntry struct {
	capture     Capture
	breaker     Breaker
	lastSuccess time.Time
	lastUsed    uint64
}

// MemState is the in-process State implementation: one capture slot plus one
// breaker per session, bounded to maxSessions and evicted least-recently-used
// first. Every method is safe for concurrent use; the zero value works because
// the session map is allocated lazily under the lock.
type MemState struct {
	mu       sync.Mutex
	sessions map[string]*sessionEntry
	seq      uint64

	// OnEvict, when set, is called with the session id whose capture and
	// breaker state the table dropped to stay within maxSessions. It runs after
	// the lock is released, so an observer reading State cannot deadlock. A
	// test or a host that does not care leaves it nil.
	OnEvict func(sessionID string)
}

var _ State = (*MemState)(nil)

// NewState returns an empty, ready-to-use session table. It needs no Close.
func NewState() *MemState {
	return &MemState{sessions: make(map[string]*sessionEntry)}
}

// Put stores the request body a session just sent. The payload is copied so a
// caller reusing its buffer cannot rewrite history; At is taken from the
// injected clock.
func (s *MemState) Put(sessionID, modelID, provider string, payload json.RawMessage) {
	s.mu.Lock()
	var evicted string
	defer func() {
		s.mu.Unlock()
		s.notifyEvicted(evicted)
	}()

	e, evicted := s.ensureLocked(sessionID)
	e.capture = Capture{
		Payload:  clonePayload(payload),
		ModelID:  modelID,
		Provider: provider,
		At:       now(),
	}
}

// Get returns a copy of the session's last capture. The payload bytes are
// cloned, so mutating the returned Capture cannot affect later reads.
func (s *MemState) Get(sessionID string) (Capture, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.touchLocked(sessionID)
	if e == nil {
		return Capture{}, false
	}
	return cloneCapture(e.capture), true
}

// Forget drops the capture and breaker for a session. Unknown sessions are a
// no-op; a genuinely new session arrives under a new sessionID.
func (s *MemState) Forget(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, sessionID)
}

// Breaker returns a copy of the session's breaker state, or the zero value for
// an unknown session.
func (s *MemState) Breaker(sessionID string) Breaker {
	s.mu.Lock()
	defer s.mu.Unlock()

	e := s.touchLocked(sessionID)
	if e == nil {
		return Breaker{}
	}
	return e.breaker
}

// NoteSuccess records a successful compaction and re-arms the breaker: it
// clears Fails and any previous Disabled flag before thrash is evaluated, so
// the very call that completes a thrash burst still ends disabled. The first
// success of a burst counts as the first strike and each further success within
// thrashWindow adds one. Time, not a turn counter, is the burst measure because
// the extension runs in its own process and never sees PiG's turn count.
func (s *MemState) NoteSuccess(sessionID string) {
	s.mu.Lock()
	var evicted string
	defer func() {
		s.mu.Unlock()
		s.notifyEvicted(evicted)
	}()

	e, evicted := s.ensureLocked(sessionID)
	e.breaker.Fails = 0
	e.breaker.Disabled = false

	t := now()
	if !e.lastSuccess.IsZero() && t.Sub(e.lastSuccess) < thrashWindow {
		e.breaker.Thrash++
	} else {
		e.breaker.Thrash = 1
	}
	if e.breaker.Thrash >= thrashLimit {
		e.breaker.Disabled = true
	}
	e.lastSuccess = t
}

// NoteFailure records a failed compaction attempt. failLimit consecutive
// failures trip the breaker; a later success re-arms it.
func (s *MemState) NoteFailure(sessionID string) {
	s.mu.Lock()
	var evicted string
	defer func() {
		s.mu.Unlock()
		s.notifyEvicted(evicted)
	}()

	e, evicted := s.ensureLocked(sessionID)
	e.breaker.Fails++
	if e.breaker.Fails >= failLimit {
		e.breaker.Disabled = true
	}
}

// ensureLocked returns the session entry, creating it (and evicting the least
// recently used session if the table is full) and marking it most recently
// used. It also returns the id of the session it evicted, empty when the table
// had room. The caller must hold s.mu.
func (s *MemState) ensureLocked(sessionID string) (*sessionEntry, string) {
	if s.sessions == nil {
		s.sessions = make(map[string]*sessionEntry)
	}
	e := s.sessions[sessionID]
	if e == nil {
		evicted := ""
		if len(s.sessions) >= maxSessions {
			evicted = s.evictLocked()
		}
		e = &sessionEntry{}
		s.sessions[sessionID] = e
		s.touchEntryLocked(e)
		return e, evicted
	}
	s.touchEntryLocked(e)
	return e, ""
}

// notifyEvicted reports an eviction to OnEvict. Callers hold no lock when they
// call it, and an unset hook is silent.
func (s *MemState) notifyEvicted(sessionID string) {
	if sessionID == "" || s.OnEvict == nil {
		return
	}
	s.OnEvict(sessionID)
}

// touchLocked marks an existing session most recently used. Unknown sessions
// are left unknown: reads must not create state.
func (s *MemState) touchLocked(sessionID string) *sessionEntry {
	e := s.sessions[sessionID]
	if e == nil {
		return nil
	}
	s.touchEntryLocked(e)
	return e
}

func (s *MemState) touchEntryLocked(e *sessionEntry) {
	s.seq++
	e.lastUsed = s.seq
}

// evictLocked removes the entry with the smallest lastUsed stamp, breaker
// state included, and returns its id. The table is tiny (maxSessions), so a
// linear scan is fine.
func (s *MemState) evictLocked() string {
	var oldestID string
	var oldest *sessionEntry
	for id, e := range s.sessions {
		if oldest == nil || e.lastUsed < oldest.lastUsed {
			oldestID = id
			oldest = e
		}
	}
	if oldest != nil {
		delete(s.sessions, oldestID)
	}
	return oldestID
}

func clonePayload(payload json.RawMessage) json.RawMessage {
	if payload == nil {
		return nil
	}
	out := make(json.RawMessage, len(payload))
	copy(out, payload)
	return out
}

func cloneCapture(c Capture) Capture {
	out := c
	out.Payload = clonePayload(c.Payload)
	return out
}

// Trace is the JSON-lines Sink. It deliberately keeps no open file handle:
// every Append opens, writes and closes, so a killed extension process still
// leaves every previously appended line on disk.
type Trace struct {
	path string
	mu   sync.Mutex
}

var _ Sink = (*Trace)(nil)

// NewTrace returns a trace sink writing to path. An empty path yields a sink
// whose Append does nothing, so callers can switch tracing off.
func NewTrace(path string) *Trace {
	return &Trace{path: path}
}

// Append marshals rec as one JSON line and appends it to the trace file,
// creating the parent directory when needed. Errors are returned, never
// panicked; Append is safe for concurrent use.
func (t *Trace) Append(rec TraceRecord) error {
	if t == nil || t.path == "" {
		return nil
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("trace marshal: %w", err)
	}
	line = append(line, '\n')

	t.mu.Lock()
	defer t.mu.Unlock()

	if dir := filepath.Dir(t.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("trace mkdir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(t.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("trace open %s: %w", t.path, err)
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return fmt.Errorf("trace write %s: %w", t.path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("trace close %s: %w", t.path, err)
	}
	return nil
}
