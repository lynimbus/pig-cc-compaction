package pigcc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is only used through the package-level now hook, so tests can walk
// across the thrash window without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func withClock(t *testing.T, c *fakeClock) {
	t.Helper()
	old := now
	now = c.Now
	t.Cleanup(func() { now = old })
}

func mustPut(t *testing.T, s *MemState, sessionID, modelID, provider, payload string) {
	t.Helper()
	s.Put(sessionID, modelID, provider, json.RawMessage(payload))
}

// --- capture -----------------------------------------------------------------

func TestPutGetRoundTrip(t *testing.T) {
	s := NewState()
	payload := "{\n  \"model\" : \"m\",\n  \"messages\" : [1, 2, 3]\n}"
	mustPut(t, s, "s1", "model-a", "provider-a", payload)

	got, ok := s.Get("s1")
	if !ok {
		t.Fatal("Get after Put returned false")
	}
	if string(got.Payload) != payload {
		t.Fatalf("payload mismatch:\n got %q\nwant %q", got.Payload, payload)
	}
	if got.ModelID != "model-a" {
		t.Fatalf("modelID = %q, want %q", got.ModelID, "model-a")
	}
	if got.Provider != "provider-a" {
		t.Fatalf("provider = %q, want %q", got.Provider, "provider-a")
	}
	if got.At.IsZero() {
		t.Fatal("Put did not stamp At")
	}
}

func TestPutUsesInjectedClockForAt(t *testing.T) {
	fixed := time.Date(2026, 2, 3, 4, 5, 6, 789, time.UTC)
	withClock(t, newFakeClock(fixed))
	s := NewState()
	mustPut(t, s, "s1", "m", "p", `{"a":1}`)

	got, ok := s.Get("s1")
	if !ok {
		t.Fatal("Get after Put returned false")
	}
	if !got.At.Equal(fixed) {
		t.Fatalf("At = %v, want %v", got.At, fixed)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	s := NewState()
	mustPut(t, s, "s1", "m", "p", `{"a":1}`)

	first, ok := s.Get("s1")
	if !ok {
		t.Fatal("Get after Put returned false")
	}
	for i := range first.Payload {
		first.Payload[i] = 'X'
	}
	first.ModelID = "mutated"
	first.Provider = "mutated"
	// Read the copy back before comparing: the writes must land on the returned
	// value, or the comparison below would pass for the wrong reason.
	if first.ModelID != "mutated" || first.Provider != "mutated" {
		t.Fatalf("caller cannot mutate its own copy: %+v", first)
	}

	second, ok := s.Get("s1")
	if !ok {
		t.Fatal("second Get returned false")
	}
	if string(second.Payload) != `{"a":1}` {
		t.Fatalf("mutation leaked into stored payload: %q", second.Payload)
	}
	if second.ModelID != "m" || second.Provider != "p" {
		t.Fatalf("mutation leaked into stored metadata: %+v", second)
	}
}

func TestPutReplacesCaptureButKeepsBreaker(t *testing.T) {
	s := NewState()
	mustPut(t, s, "s1", "m1", "p1", `{"a":1}`)
	s.NoteFailure("s1")

	mustPut(t, s, "s1", "m2", "p2", `{"b":2}`)

	got, ok := s.Get("s1")
	if !ok {
		t.Fatal("Get after second Put returned false")
	}
	if string(got.Payload) != `{"b":2}` || got.ModelID != "m2" || got.Provider != "p2" {
		t.Fatalf("second Put did not replace capture: %+v", got)
	}
	if b := s.Breaker("s1"); b.Fails != 1 {
		t.Fatalf("second Put reset breaker: %+v, want Fails=1", b)
	}
}

func TestGetUnknownSession(t *testing.T) {
	s := NewState()
	got, ok := s.Get("missing")
	if ok {
		t.Fatal("Get on unknown session returned true")
	}
	if got.Payload != nil || got.ModelID != "" || got.Provider != "" || !got.At.IsZero() {
		t.Fatalf("unknown session returned non-zero capture: %+v", got)
	}
}

// --- breaker -----------------------------------------------------------------

func TestNoteFailureTripsAtThirdStrike(t *testing.T) {
	s := NewState()
	mustPut(t, s, "s1", "m", "p", `{}`)

	for i := 1; i <= failLimit; i++ {
		s.NoteFailure("s1")
		b := s.Breaker("s1")
		if b.Fails != i {
			t.Fatalf("after failure %d: Fails = %d, want %d", i, b.Fails, i)
		}
		wantDisabled := i >= failLimit
		if b.Disabled != wantDisabled {
			t.Fatalf("after failure %d: Disabled = %v, want %v", i, b.Disabled, wantDisabled)
		}
	}
}

func TestNoteSuccessResetsFails(t *testing.T) {
	s := NewState()
	mustPut(t, s, "s1", "m", "p", `{}`)

	s.NoteFailure("s1")
	s.NoteFailure("s1")
	if b := s.Breaker("s1"); b.Fails != 2 || b.Disabled {
		t.Fatalf("before success: %+v, want Fails=2 Disabled=false", b)
	}

	s.NoteSuccess("s1")
	b := s.Breaker("s1")
	if b.Fails != 0 {
		t.Fatalf("Fails after success = %d, want 0", b.Fails)
	}
	if b.Disabled {
		t.Fatalf("a single success opened the breaker: %+v", b)
	}

	// The reset must be real: a full new run of failures is needed again.
	for i := 1; i < failLimit; i++ {
		s.NoteFailure("s1")
		if got := s.Breaker("s1"); got.Disabled {
			t.Fatalf("tripped early at failure %d after success reset: %+v", i, got)
		}
	}
	s.NoteFailure("s1")
	if b := s.Breaker("s1"); !b.Disabled {
		t.Fatalf("breaker not tripped by %d fresh failures: %+v", failLimit, b)
	}
}

func TestThrashWindowTripsBreaker(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	withClock(t, clock)
	s := NewState()
	mustPut(t, s, "s1", "m", "p", `{}`)

	// Three successes inside the window are a thrash burst.
	for i := 1; i <= thrashLimit; i++ {
		s.NoteSuccess("s1")
		b := s.Breaker("s1")
		if wantDisabled := i >= thrashLimit; b.Disabled != wantDisabled {
			t.Fatalf("after success %d: Disabled = %v, want %v (breaker %+v)", i, b.Disabled, wantDisabled, b)
		}
		if b.Thrash != i {
			t.Fatalf("after success %d: Thrash = %d, want %d", i, b.Thrash, i)
		}
		clock.Advance(10 * time.Second)
	}
	if b := s.Breaker("s1"); !b.Disabled || b.Thrash < thrashLimit {
		t.Fatalf("thrash burst did not disable: %+v", b)
	}
}

func TestSpacedSuccessesDoNotTripBreaker(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	withClock(t, clock)
	s := NewState()
	mustPut(t, s, "s1", "m", "p", `{}`)

	for i := 0; i < 5; i++ {
		s.NoteSuccess("s1")
		if b := s.Breaker("s1"); b.Disabled {
			t.Fatalf("spaced successes disabled the breaker at %d: %+v", i+1, b)
		}
		clock.Advance(31 * time.Second)
	}
	if b := s.Breaker("s1"); b.Thrash != 1 {
		t.Fatalf("Thrash after spaced successes = %d, want 1", b.Thrash)
	}
}

func TestSuccessReArmsBreaker(t *testing.T) {
	s := NewState()
	mustPut(t, s, "s1", "m", "p", `{}`)

	for i := 0; i < failLimit; i++ {
		s.NoteFailure("s1")
	}
	if b := s.Breaker("s1"); !b.Disabled || b.Fails != failLimit {
		t.Fatalf("after %d failures: %+v, want Disabled=true Fails=%d", failLimit, b, failLimit)
	}

	s.NoteSuccess("s1")
	if b := s.Breaker("s1"); b.Disabled || b.Fails != 0 || b.Thrash != 1 {
		t.Fatalf("success did not re-arm breaker: %+v, want Disabled=false Fails=0 Thrash=1", b)
	}

	s.NoteFailure("s1")
	if b := s.Breaker("s1"); b.Fails != 1 || b.Disabled {
		t.Fatalf("re-armed breaker did not count next failure: %+v, want Fails=1 Disabled=false", b)
	}
}

func TestSuccessAfterThrashWindowClearsBreaker(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	withClock(t, clock)
	s := NewState()
	mustPut(t, s, "s1", "m", "p", `{}`)

	for i := 1; i <= thrashLimit; i++ {
		s.NoteSuccess("s1")
		if i < thrashLimit {
			clock.Advance(10 * time.Second)
		}
	}
	// The call that completes the burst must end disabled: the re-arm happens
	// before the thrash evaluation, so it cannot mask the trip.
	if b := s.Breaker("s1"); !b.Disabled || b.Thrash != thrashLimit {
		t.Fatalf("thrash burst did not trip on its final success: %+v", b)
	}

	clock.Advance(thrashWindow + time.Second)
	s.NoteSuccess("s1")

	b := s.Breaker("s1")
	if b.Disabled {
		t.Fatalf("success past the window did not clear Disabled: %+v", b)
	}
	if b.Thrash != 1 {
		t.Fatalf("Thrash after window-clearing success = %d, want 1", b.Thrash)
	}
	if b.Fails != 0 {
		t.Fatalf("Fails after success = %d, want 0", b.Fails)
	}
}

func TestBreakerUnknownSession(t *testing.T) {
	s := NewState()
	if b := s.Breaker("missing"); b != (Breaker{}) {
		t.Fatalf("Breaker(unknown) = %+v, want zero", b)
	}
}

// --- forget ------------------------------------------------------------------

func TestForgetDropsCaptureAndBreaker(t *testing.T) {
	s := NewState()
	mustPut(t, s, "s1", "m", "p", `{"a":1}`)
	s.NoteFailure("s1")
	s.NoteFailure("s1")

	s.Forget("s1")

	if _, ok := s.Get("s1"); ok {
		t.Fatal("Get after Forget returned true")
	}
	if b := s.Breaker("s1"); b != (Breaker{}) {
		t.Fatalf("Breaker after Forget = %+v, want zero", b)
	}
}

func TestForgetUnknownSessionIsNoop(t *testing.T) {
	s := NewState()
	s.Forget("missing")
	if _, ok := s.Get("missing"); ok {
		t.Fatal("Get after forgetting unknown session returned true")
	}
}

// --- LRU ---------------------------------------------------------------------

func TestSessionLRUEvictsOldest(t *testing.T) {
	s := NewState()
	for i := 0; i <= maxSessions; i++ {
		sid := fmt.Sprintf("s%02d", i)
		mustPut(t, s, sid, "m", "p", fmt.Sprintf(`{"i":%d}`, i))
	}

	if _, ok := s.Get("s00"); ok {
		t.Fatal("oldest session was not evicted")
	}
	for i := 1; i <= maxSessions; i++ {
		sid := fmt.Sprintf("s%02d", i)
		got, ok := s.Get(sid)
		if !ok {
			t.Fatalf("session %s missing after eviction of s00", sid)
		}
		if want := fmt.Sprintf(`{"i":%d}`, i); string(got.Payload) != want {
			t.Fatalf("session %s payload = %q, want %q", sid, got.Payload, want)
		}
	}
}

// TestEvictionIsReported: the table tells the observer which session it dropped,
// once per eviction, and only when something is really dropped.
func TestEvictionIsReported(t *testing.T) {
	s := NewState()
	var evicted []string
	s.OnEvict = func(sessionID string) { evicted = append(evicted, sessionID) }

	for i := 0; i < maxSessions; i++ {
		mustPut(t, s, fmt.Sprintf("s%02d", i), "m", "p", `{"i":0}`)
	}
	if len(evicted) != 0 {
		t.Fatalf("evicted %v while the table still had room", evicted)
	}

	// Whichever session was touched most recently is not the one to go: s00 is
	// refreshed, so s01 becomes the oldest.
	mustPut(t, s, "s00", "m", "p", `{"i":1}`)
	mustPut(t, s, "s99", "m", "p", `{"i":2}`)
	if want := []string{"s01"}; !slices.Equal(evicted, want) {
		t.Fatalf("evicted = %v, want %v", evicted, want)
	}

	// Forget removes a session on purpose and is not an eviction.
	s.Forget("s00")
	if len(evicted) != 1 {
		t.Fatalf("evicted = %v after Forget, want no further entries", evicted)
	}
}

// TestBreakerWritesAlsoReportEviction: NoteSuccess and NoteFailure create the
// session entry too, so they must not drop one silently.
func TestBreakerWritesAlsoReportEviction(t *testing.T) {
	s := NewState()
	for i := 0; i < maxSessions; i++ {
		mustPut(t, s, fmt.Sprintf("s%02d", i), "m", "p", `{"i":0}`)
	}
	var evicted []string
	s.OnEvict = func(sessionID string) { evicted = append(evicted, sessionID) }
	s.NoteFailure("fresh")
	if want := []string{"s00"}; !slices.Equal(evicted, want) {
		t.Fatalf("evicted = %v, want %v", evicted, want)
	}
}

func TestLRURefreshesLastUsed(t *testing.T) {
	ops := map[string]func(s *MemState){
		"Put": func(s *MemState) {
			s.Put("s00", "m", "p", json.RawMessage(`{"touched":true}`))
		},
		"Get": func(s *MemState) {
			s.Get("s00")
		},
		"Breaker": func(s *MemState) {
			s.Breaker("s00")
		},
		"NoteSuccess": func(s *MemState) {
			s.NoteSuccess("s00")
		},
		"NoteFailure": func(s *MemState) {
			s.NoteFailure("s00")
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			s := NewState()
			for i := 0; i < maxSessions; i++ {
				sid := fmt.Sprintf("s%02d", i)
				mustPut(t, s, sid, "m", "p", fmt.Sprintf(`{"i":%d}`, i))
			}
			op(s)
			mustPut(t, s, "s32", "m", "p", `{"i":32}`)

			if _, ok := s.Get("s00"); !ok {
				t.Fatalf("%s did not refresh lastUsed: s00 was evicted", name)
			}
			if _, ok := s.Get("s01"); ok {
				t.Fatalf("%s: expected s01 (the untouched oldest) to be evicted", name)
			}
		})
	}
}

func TestEvictionDropsBreakerState(t *testing.T) {
	s := NewState()
	mustPut(t, s, "s00", "m", "p", `{"i":0}`)
	s.NoteFailure("s00")
	s.NoteFailure("s00")

	for i := 1; i < maxSessions; i++ {
		mustPut(t, s, fmt.Sprintf("s%02d", i), "m", "p", fmt.Sprintf(`{"i":%d}`, i))
	}
	mustPut(t, s, "s32", "m", "p", `{"i":32}`)

	if _, ok := s.Get("s00"); ok {
		t.Fatal("s00 was not evicted")
	}
	if b := s.Breaker("s00"); b != (Breaker{}) {
		t.Fatalf("breaker state survived eviction: %+v", b)
	}
}

// --- trace -------------------------------------------------------------------

func TestTraceAppendRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "trace.jsonl")
	tr := NewTrace(path)

	rec := TraceRecord{
		TS:              time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		SessionID:       "sess-1",
		Outcome:         OutcomeCompacted,
		Detail:          "summary written",
		InputTokens:     1234,
		CacheReadTokens: 4096,
		OutputTokens:    512,
		DurationMs:      9876,
	}
	if err := tr.Append(rec); err != nil {
		t.Fatalf("Append: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("trace line not newline terminated: %q", data)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1:\n%s", len(lines), data)
	}

	var got TraceRecord
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("line is not valid JSON: %v\n%s", err, lines[0])
	}
	if !got.TS.Equal(rec.TS) {
		t.Fatalf("TS = %v, want %v", got.TS, rec.TS)
	}
	if got.SessionID != rec.SessionID || got.Outcome != rec.Outcome || got.Detail != rec.Detail {
		t.Fatalf("metadata mismatch: %+v, want %+v", got, rec)
	}
	if got.InputTokens != rec.InputTokens || got.CacheReadTokens != rec.CacheReadTokens || got.OutputTokens != rec.OutputTokens {
		t.Fatalf("token mismatch: %+v, want %+v", got, rec)
	}
	if got.DurationMs != rec.DurationMs {
		t.Fatalf("DurationMs = %d, want %d", got.DurationMs, rec.DurationMs)
	}
}

func TestTraceAppendsWithoutTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tr := NewTrace(path)
	for i := 0; i < 3; i++ {
		rec := TraceRecord{TS: time.Now(), SessionID: fmt.Sprintf("s%d", i), Outcome: OutcomeFallback}
		if err := tr.Append(rec); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), data)
	}
	for i, line := range lines {
		var rec TraceRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d invalid JSON: %v", i, err)
		}
		if want := fmt.Sprintf("s%d", i); rec.SessionID != want {
			t.Fatalf("line %d SessionID = %q, want %q", i, rec.SessionID, want)
		}
	}
}

func TestTraceEmptyPathIsNoop(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	tr := NewTrace("")
	if err := tr.Append(TraceRecord{SessionID: "s", Outcome: OutcomeFailed}); err != nil {
		t.Fatalf("Append with empty path returned error: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty-path trace created files: %v", entries)
	}
}

func TestTraceAppendErrorDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("setup blocker: %v", err)
	}
	tr := NewTrace(filepath.Join(blocker, "trace.jsonl"))
	if err := tr.Append(TraceRecord{SessionID: "s"}); err == nil {
		t.Fatal("Append under a file-as-directory returned nil error")
	}
}

func TestTraceConcurrentAppendExactly100(t *testing.T) {
	const total = 100
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tr := NewTrace(path)

	var next atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, total)
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if i >= total {
					return
				}
				rec := TraceRecord{
					TS:          time.Now(),
					SessionID:   fmt.Sprintf("s%03d", i),
					Outcome:     OutcomeCompacted,
					InputTokens: int(i),
				}
				if err := tr.Append(rec); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Append: %v", err)
	}

	lines := readTraceLines(t, path)
	if len(lines) != total {
		t.Fatalf("got %d lines, want %d", len(lines), total)
	}
	seen := make(map[string]bool, total)
	for i, line := range lines {
		var rec TraceRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d invalid JSON: %v", i, err)
		}
		if seen[rec.SessionID] {
			t.Fatalf("duplicate record %q at line %d", rec.SessionID, i)
		}
		seen[rec.SessionID] = true
	}
}

func TestTraceConcurrentAppendPerGoroutine(t *testing.T) {
	const goroutines, perGoroutine = 16, 100
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	tr := NewTrace(path)

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				rec := TraceRecord{
					TS:           time.Now(),
					SessionID:    fmt.Sprintf("g%02d-i%03d", g, i),
					Outcome:      OutcomeFallback,
					OutputTokens: i,
				}
				if err := tr.Append(rec); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Append: %v", err)
	}

	lines := readTraceLines(t, path)
	if len(lines) != goroutines*perGoroutine {
		t.Fatalf("got %d lines, want %d", len(lines), goroutines*perGoroutine)
	}
	for i, line := range lines {
		var rec TraceRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d invalid JSON: %v", i, err)
		}
	}
}

// --- concurrency -------------------------------------------------------------

func TestStateAndTraceConcurrentMixed(t *testing.T) {
	const goroutines = 16
	const iterations = 200
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	s := NewState()
	tr := NewTrace(path)

	var wg sync.WaitGroup
	errs := make(chan error, goroutines*iterations)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			sid := fmt.Sprintf("s%02d", g)
			payload := json.RawMessage(fmt.Sprintf(`{"goroutine":%d}`, g))
			for i := 0; i < iterations; i++ {
				switch i % 5 {
				case 0:
					s.Put(sid, "model", "provider", payload)
				case 1:
					if got, ok := s.Get(sid); ok && string(got.Payload) != string(payload) {
						errs <- fmt.Errorf("session %s payload changed: %q", sid, got.Payload)
						return
					}
				case 2:
					s.NoteFailure(sid)
				case 3:
					s.NoteSuccess(sid)
				case 4:
					rec := TraceRecord{
						TS:          time.Now(),
						SessionID:   sid,
						Outcome:     OutcomeCompacted,
						InputTokens: i,
					}
					if err := tr.Append(rec); err != nil {
						errs <- err
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("mixed concurrency: %v", err)
	}

	wantLines := goroutines * (iterations / 5)
	lines := readTraceLines(t, path)
	if len(lines) != wantLines {
		t.Fatalf("got %d trace lines, want %d", len(lines), wantLines)
	}
	for i, line := range lines {
		var rec TraceRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d invalid JSON: %v", i, err)
		}
	}
}

func readTraceLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan trace: %v", err)
	}
	return lines
}
