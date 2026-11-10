package pigcc

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	t3TestSessionID = "session-t3"
	t3TestModelID   = "deepseek"
	t3TestProvider  = "magpie"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type t3PutCall struct {
	sessionID string
	modelID   string
	provider  string
	payload   json.RawMessage
}

type t3State struct {
	mu        sync.Mutex
	captures  map[string]Capture
	breakers  map[string]Breaker
	puts      []t3PutCall
	successes int
	failures  int
}

func newT3State() *t3State {
	return &t3State{
		captures: make(map[string]Capture),
		breakers: make(map[string]Breaker),
	}
}

func (s *t3State) Put(sessionID, modelID, provider string, payload json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := append(json.RawMessage(nil), payload...)
	s.puts = append(s.puts, t3PutCall{
		sessionID: sessionID,
		modelID:   modelID,
		provider:  provider,
		payload:   append(json.RawMessage(nil), stored...),
	})
	s.captures[sessionID] = Capture{
		Payload:  stored,
		ModelID:  modelID,
		Provider: provider,
		At:       time.Now(),
	}
}

func (s *t3State) Get(sessionID string) (Capture, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.captures[sessionID]
	if !ok {
		return Capture{}, false
	}
	c.Payload = append(json.RawMessage(nil), c.Payload...)
	return c, true
}

func (s *t3State) Forget(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.captures, sessionID)
	delete(s.breakers, sessionID)
}

func (s *t3State) Breaker(sessionID string) Breaker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.breakers[sessionID]
}

func (s *t3State) NoteSuccess(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.successes++
	b := s.breakers[sessionID]
	b.Fails = 0
	s.breakers[sessionID] = b
}

func (s *t3State) NoteFailure(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	b := s.breakers[sessionID]
	b.Fails++
	s.breakers[sessionID] = b
}

func (s *t3State) setCapture(sessionID string, c Capture) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captures[sessionID] = c
}

func (s *t3State) putCalls() []t3PutCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]t3PutCall(nil), s.puts...)
}

func (s *t3State) failureCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures
}

func (s *t3State) successCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.successes
}

type t3Plugin struct {
	mu          sync.Mutex
	planFn      func(context.Context, PlanInput) (PlanOutput, error)
	parseFn     func(context.Context, ParseInput) (ParseOutput, error)
	planInputs  []PlanInput
	parseInputs []ParseInput
}

func (p *t3Plugin) Plan(ctx context.Context, in PlanInput) (PlanOutput, error) {
	p.mu.Lock()
	p.planInputs = append(p.planInputs, in)
	fn := p.planFn
	p.mu.Unlock()
	if fn == nil {
		return PlanOutput{}, nil
	}
	return fn(ctx, in)
}

func (p *t3Plugin) Parse(ctx context.Context, in ParseInput) (ParseOutput, error) {
	p.mu.Lock()
	p.parseInputs = append(p.parseInputs, in)
	fn := p.parseFn
	p.mu.Unlock()
	if fn == nil {
		return ParseOutput{}, nil
	}
	return fn(ctx, in)
}

func (p *t3Plugin) plans() []PlanInput {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]PlanInput(nil), p.planInputs...)
}

func (p *t3Plugin) parses() []ParseInput {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ParseInput(nil), p.parseInputs...)
}

type t3Sink struct {
	mu   sync.Mutex
	recs []TraceRecord
	err  error
}

func (s *t3Sink) Append(rec TraceRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, rec)
	return s.err
}

func (s *t3Sink) records() []TraceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TraceRecord(nil), s.recs...)
}

type t3StreamCall struct {
	model     map[string]any
	request   map[string]any
	onPayload func(any, map[string]any) (any, error)
}

type t3Stream struct {
	mu    sync.Mutex
	calls []t3StreamCall
	fn    func(call int, model, request map[string]any, onPayload func(any, map[string]any) (any, error)) (map[string]any, error)
}

func (s *t3Stream) asFunc() StreamFunc {
	return func(model map[string]any, request map[string]any, onPayload func(any, map[string]any) (any, error)) (map[string]any, error) {
		s.mu.Lock()
		call := len(s.calls)
		s.calls = append(s.calls, t3StreamCall{model: model, request: request, onPayload: onPayload})
		fn := s.fn
		s.mu.Unlock()
		if fn == nil {
			return map[string]any{}, nil
		}
		return fn(call, model, request, onPayload)
	}
}

func (s *t3Stream) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *t3Stream) callsCopy() []t3StreamCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]t3StreamCall(nil), s.calls...)
}

type t3SleepRecorder struct {
	mu     sync.Mutex
	delays []time.Duration
	fn     func(context.Context, time.Duration) error
}

func (s *t3SleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.delays = append(s.delays, d)
	fn := s.fn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, d)
	}
	return nil
}

func (s *t3SleepRecorder) calls() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.delays...)
}

// ---------------------------------------------------------------------------
// Harness and fixtures
// ---------------------------------------------------------------------------

type t3Harness struct {
	state  *t3State
	plugin *t3Plugin
	sink   *t3Sink
	stream *t3Stream
	sleep  *t3SleepRecorder
	driver *Driver
	now    time.Time
}

func newT3Harness(
	t *testing.T,
	planFn func(context.Context, PlanInput) (PlanOutput, error),
	parseFn func(context.Context, ParseInput) (ParseOutput, error),
	streamFn func(int, map[string]any, map[string]any, func(any, map[string]any) (any, error)) (map[string]any, error),
) *t3Harness {
	t.Helper()
	h := &t3Harness{
		state:  newT3State(),
		plugin: &t3Plugin{planFn: planFn, parseFn: parseFn},
		sink:   &t3Sink{},
		stream: &t3Stream{fn: streamFn},
		sleep:  &t3SleepRecorder{},
		now:    time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
	}
	h.driver = NewDriver(DriverOptions{
		Plugin: h.plugin,
		State:  h.state,
		Trace:  h.sink,
		Stream: h.stream.asFunc(),
		FindModel: func(provider, modelID string) (map[string]any, bool) {
			return map[string]any{"provider": provider, "id": modelID}, true
		},
		Now:   func() time.Time { return h.now },
		Sleep: h.sleep.sleep,
	})
	return h
}

var (
	t3PlanRequest = json.RawMessage(`{"model":"deepseek","stream":true}`)
	t3PlanPayload = json.RawMessage(`{"model":"deepseek","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"user","content":[{"type":"text","text":"Summarize the conversation."}]}],"max_tokens":4096}`)
)

func t3TakeoverPlan() PlanOutput {
	return PlanOutput{
		Action:  "takeover",
		Request: t3PlanRequest,
		Payload: t3PlanPayload,
	}
}

// TestBeforeCompactAppendsFileOperations: the summary PiG stores carries the same
// <read-files>/<modified-files> tail its own compaction writes, merged with what
// the previous takeover left behind, and details records the two lists.
func TestBeforeCompactAppendsFileOperations(t *testing.T) {
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
		func(context.Context, ParseInput) (ParseOutput, error) { return ParseOutput{Summary: "body"}, nil },
		nil,
	)

	ev := t3CompactEvent()
	ev.Preparation.FileOps = FileOps{
		Read:    []string{"b.go", "a.go"},
		Written: []string{"c.go"},
		Edited:  []string{"d.go"},
	}
	// The previous takeover listed files this compaction's own operations no
	// longer mention. b.go is in both lists and must survive as a read.
	ev.Preparation.PreviousSummary = "older\n\n<read-files>\nold.go\nb.go\n</read-files>\n\n" +
		"<modified-files>\nc.go\n</modified-files>"

	got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, ev)
	assertT3Compaction(t, got, "body\n\n<read-files>\na.go\nb.go\nold.go\n</read-files>\n\n"+
		"<modified-files>\nc.go\nd.go\n</modified-files>", "e9", 42000)

	compaction, _ := got["compaction"].(map[string]any)
	details, ok := compaction["details"].(map[string]any)
	if !ok {
		t.Fatalf("details = %#v, want a map", compaction["details"])
	}
	if want := []string{"a.go", "b.go", "old.go"}; !slices.Equal(details["readFiles"].([]string), want) {
		t.Errorf("readFiles = %#v, want %#v", details["readFiles"], want)
	}
	if want := []string{"c.go", "d.go"}; !slices.Equal(details["modifiedFiles"].([]string), want) {
		t.Errorf("modifiedFiles = %#v, want %#v", details["modifiedFiles"], want)
	}

	// No files touched means no tail and no details key at all.
	plain := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	assertT3Compaction(t, plain, "body", "e9", 42000)
	if bare := plain["compaction"].(map[string]any); bare["details"] != nil {
		t.Errorf("details = %#v, want absent when no file was touched", bare["details"])
	}
}

func t3CompactEvent() CompactEvent {
	return CompactEvent{
		Reason:             "threshold",
		CustomInstructions: "keep api details",
		WillRetry:          false,
		Preparation: Preparation{
			FirstKeptEntryID: "e9",
			TokensBefore:     42000,
			PreviousSummary:  "older summary",
			IsSplitTurn:      true,
			Settings:         Settings{Enabled: true, ReserveTokens: 1000, KeepRecentTokens: 2000},
		},
	}
}

func t3SuccessResponse(text string) map[string]any {
	return map[string]any{
		"content": []any{
			map[string]any{"type": "text", "text": text},
		},
		"stopReason": "end_turn",
		"usage": map[string]any{
			"input":     float64(9000),
			"cacheRead": float64(8192),
			"output":    float64(640),
		},
	}
}

func assertT3Compaction(t *testing.T, got map[string]any, summary, firstKept string, tokensBefore int) {
	t.Helper()
	if got == nil {
		t.Fatalf("BeforeCompact returned nil, want a compaction result")
	}
	if len(got) != 1 {
		t.Fatalf("result has %d top-level keys, want 1: %#v", len(got), got)
	}
	raw, ok := got["compaction"]
	if !ok {
		t.Fatalf("result missing compaction key: %#v", got)
	}
	compaction, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("compaction is %T, want map[string]any", raw)
	}
	if got := compaction["summary"]; got != summary {
		t.Errorf("summary = %#v, want %q", got, summary)
	}
	if got := compaction["firstKeptEntryId"]; got != firstKept {
		t.Errorf("firstKeptEntryId = %#v, want %q", got, firstKept)
	}
	gotTokens, ok := compaction["tokensBefore"].(int)
	if !ok {
		t.Fatalf("tokensBefore is %T, want int", compaction["tokensBefore"])
	}
	if gotTokens != tokensBefore {
		t.Errorf("tokensBefore = %d, want %d", gotTokens, tokensBefore)
	}
}

// ---------------------------------------------------------------------------
// Construction / capture
// ---------------------------------------------------------------------------

// TestT3NewDriverSubstitutesMissingDependencies: the constructor is total. A nil
// plugin is the policy-unavailable case (a load error, which the extension
// reports once at session start) and declines instead of crashing; a nil state
// gets a working one.
func TestT3NewDriverSubstitutesMissingDependencies(t *testing.T) {
	declining := NewDriver(DriverOptions{State: newT3State()})
	if got := declining.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent()); got != nil {
		t.Fatalf("BeforeCompact without a plugin = %#v, want nil", got)
	}
	// A declining policy is not a failure: the breaker must stay untouched, or a
	// policy that never loaded would trip it after three compactions.
	if fails := declining.state.Breaker(t3TestSessionID).Fails; fails != 0 {
		t.Errorf("Breaker.Fails = %d, want 0 for a plain decline", fails)
	}

	defaulted := NewDriver(DriverOptions{Plugin: &t3Plugin{}})
	if defaulted.state == nil {
		t.Fatal("state = nil, want a default table")
	}
	defaulted.BeforeProviderRequest(t3TestSessionID, t3TestModelID, t3TestProvider, t3PlanPayload)
	if _, ok := defaulted.state.Get(t3TestSessionID); !ok {
		t.Error("capture was dropped, want it kept in the default state")
	}
}

func TestT3NewDriverFillsDefaults(t *testing.T) {
	d := NewDriver(DriverOptions{Plugin: &t3Plugin{}, State: newT3State()})
	if d.now == nil {
		t.Error("now is nil, want time.Now default")
	}
	if d.sleep == nil {
		t.Error("sleep is nil, want context-aware default")
	}
	if d.trace != nil {
		t.Error("trace default should stay nil so trace writing is skipped")
	}
}

func TestT3BeforeProviderRequestStoresPayloadVerbatim(t *testing.T) {
	state := newT3State()
	d := NewDriver(DriverOptions{Plugin: &t3Plugin{}, State: state})

	payload := map[string]any{
		"model": "deepseek",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"max_tokens": float64(1234),
	}
	if got := d.BeforeProviderRequest("s1", "m1", "p1", payload); got != nil {
		t.Fatalf("BeforeProviderRequest returned %#v, want nil", got)
	}

	want, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal expected payload: %v", err)
	}
	cap, ok := state.Get("s1")
	if !ok {
		t.Fatal("State.Get did not find the captured payload")
	}
	if !reflect.DeepEqual(cap.Payload, json.RawMessage(want)) {
		t.Errorf("captured payload = %s, want %s", cap.Payload, want)
	}
	if cap.ModelID != "m1" || cap.Provider != "p1" {
		t.Errorf("capture identity = %q/%q, want m1/p1", cap.ModelID, cap.Provider)
	}
	if got := state.putCalls(); len(got) != 1 || got[0].sessionID != "s1" {
		t.Errorf("Put calls = %#v, want one for s1", got)
	}
}

func TestT3BeforeProviderRequestSkipsNilAndUnmarshalable(t *testing.T) {
	state := newT3State()
	d := NewDriver(DriverOptions{Plugin: &t3Plugin{}, State: state})

	if got := d.BeforeProviderRequest("s1", "m1", "p1", nil); got != nil {
		t.Fatalf("nil payload returned %#v, want nil", got)
	}
	if got := d.BeforeProviderRequest("s1", "m1", "p1", func() {}); got != nil {
		t.Fatalf("unmarshalable payload returned %#v, want nil", got)
	}
	if got := state.putCalls(); len(got) != 0 {
		t.Errorf("Put was called %d times, want 0", len(got))
	}
}

// ---------------------------------------------------------------------------
// BeforeCompact decisions
// ---------------------------------------------------------------------------

func TestT3BeforeCompactPlanPassReturnsNilWithoutStreaming(t *testing.T) {
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) {
			return PlanOutput{Action: "pass", Why: "no captured request"}, nil
		},
		nil,
		nil,
	)

	got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	if got != nil {
		t.Fatalf("BeforeCompact returned %#v, want nil", got)
	}
	if n := h.stream.callCount(); n != 0 {
		t.Errorf("Stream called %d times, want 0", n)
	}
	recs := h.sink.records()
	if len(recs) != 1 {
		t.Fatalf("trace records = %d, want 1", len(recs))
	}
	if recs[0].Outcome != OutcomeFallback {
		t.Errorf("outcome = %q, want %q", recs[0].Outcome, OutcomeFallback)
	}
	if recs[0].Detail != "no captured request" {
		t.Errorf("detail = %q, want %q", recs[0].Detail, "no captured request")
	}
	if recs[0].SessionID != t3TestSessionID {
		t.Errorf("sessionId = %q, want %q", recs[0].SessionID, t3TestSessionID)
	}
	if fails := h.state.Breaker(t3TestSessionID).Fails; fails != 0 {
		t.Errorf("Breaker.Fails = %d, want 0 for an intentional pass", fails)
	}
	if n := h.state.failureCount(); n != 0 {
		t.Errorf("NoteFailure called %d times, want 0", n)
	}

	plans := h.plugin.plans()
	if len(plans) != 1 {
		t.Fatalf("Plan calls = %d, want 1", len(plans))
	}
	if plans[0].Capture != nil {
		t.Errorf("PlanInput.Capture = %#v, want nil on a state miss", plans[0].Capture)
	}
	if plans[0].Attempt != 0 {
		t.Errorf("PlanInput.Attempt = %d, want 0", plans[0].Attempt)
	}
	if plans[0].Reason != "threshold" {
		t.Errorf("PlanInput.Reason = %q, want threshold", plans[0].Reason)
	}
	if plans[0].CustomInstructions != "keep api details" {
		t.Errorf("PlanInput.CustomInstructions = %q, want keep api details", plans[0].CustomInstructions)
	}
	if plans[0].Preparation.FirstKeptEntryID != "e9" || plans[0].Preparation.TokensBefore != 42000 {
		t.Errorf("PlanInput.Preparation = %#v, want e9/42000", plans[0].Preparation)
	}
}

func TestT3BeforeCompactTakeoverSummary(t *testing.T) {
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) {
			return t3TakeoverPlan(), nil
		},
		func(context.Context, ParseInput) (ParseOutput, error) {
			return ParseOutput{Summary: "1. Primary Request"}, nil
		},
		func(int, map[string]any, map[string]any, func(any, map[string]any) (any, error)) (map[string]any, error) {
			return t3SuccessResponse("part one\npart two"), nil
		},
	)
	h.state.setCapture(t3TestSessionID, Capture{
		Payload:  json.RawMessage(`{"messages":[{"role":"user","content":"captured"}]}`),
		ModelID:  "captured-model",
		Provider: "captured-provider",
		At:       h.now.Add(-1500 * time.Millisecond),
	})

	got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	assertT3Compaction(t, got, "1. Primary Request", "e9", 42000)

	plans := h.plugin.plans()
	if len(plans) != 1 {
		t.Fatalf("Plan calls = %d, want 1", len(plans))
	}
	capture := plans[0].Capture
	if capture == nil {
		t.Fatal("PlanInput.Capture is nil, want the captured request")
	}
	if !reflect.DeepEqual(capture.Payload, json.RawMessage(`{"messages":[{"role":"user","content":"captured"}]}`)) {
		t.Errorf("capture payload = %s", capture.Payload)
	}
	if capture.ModelID != "captured-model" || capture.Provider != "captured-provider" {
		t.Errorf("capture identity = %q/%q", capture.ModelID, capture.Provider)
	}
	if capture.AgeMs != 1500 {
		t.Errorf("capture age = %dms, want 1500ms", capture.AgeMs)
	}
	if plans[0].Preparation.FirstKeptEntryID != "e9" || plans[0].Preparation.TokensBefore != 42000 {
		t.Errorf("PlanInput.Preparation = %#v", plans[0].Preparation)
	}
	if plans[0].Preparation.PreviousSummary != "older summary" {
		t.Errorf("PlanInput.Preparation.PreviousSummary = %q", plans[0].Preparation.PreviousSummary)
	}

	parses := h.plugin.parses()
	if len(parses) != 1 {
		t.Fatalf("Parse calls = %d, want 1", len(parses))
	}
	if parses[0].Text != "part one\npart two" {
		t.Errorf("ParseInput.Text = %q, want part one\\npart two", parses[0].Text)
	}
	if parses[0].StopReason != "end_turn" {
		t.Errorf("ParseInput.StopReason = %q, want end_turn", parses[0].StopReason)
	}
	if parses[0].OutputTokens != 640 {
		t.Errorf("ParseInput.OutputTokens = %d, want 640", parses[0].OutputTokens)
	}
	if parses[0].Attempt != 0 {
		t.Errorf("ParseInput.Attempt = %d, want 0", parses[0].Attempt)
	}

	recs := h.sink.records()
	if len(recs) != 1 {
		t.Fatalf("trace records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Outcome != OutcomeCompacted {
		t.Errorf("outcome = %q, want %q", rec.Outcome, OutcomeCompacted)
	}
	if rec.InputTokens != 9000 || rec.CacheReadTokens != 8192 || rec.OutputTokens != 640 {
		t.Errorf("trace tokens = %d/%d/%d, want 9000/8192/640", rec.InputTokens, rec.CacheReadTokens, rec.OutputTokens)
	}
	if !rec.TS.Equal(h.now) {
		t.Errorf("trace TS = %v, want %v", rec.TS, h.now)
	}
	if rec.DurationMs < 0 {
		t.Errorf("trace duration = %dms, want >= 0", rec.DurationMs)
	}
	if successes := h.state.successCount(); successes != 1 {
		t.Errorf("NoteSuccess calls = %d, want 1", successes)
	}
	if fails := h.state.Breaker(t3TestSessionID).Fails; fails != 0 {
		t.Errorf("Breaker.Fails = %d, want 0", fails)
	}
}

func TestT3BeforeCompactOnPayloadReplacesHostPayload(t *testing.T) {
	t.Run("decoded plan payload", func(t *testing.T) {
		var (
			mu       sync.Mutex
			payloads []any
			errs     []error
		)
		h := newT3Harness(t,
			func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
			func(context.Context, ParseInput) (ParseOutput, error) {
				return ParseOutput{Summary: "summary"}, nil
			},
			func(_ int, _ map[string]any, _ map[string]any, onPayload func(any, map[string]any) (any, error)) (map[string]any, error) {
				got, err := onPayload(map[string]any{"host": "body", "model": "rewritten"}, map[string]any{"id": "host-model"})
				mu.Lock()
				payloads = append(payloads, got)
				errs = append(errs, err)
				mu.Unlock()
				return t3SuccessResponse("summary"), nil
			},
		)

		got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
		assertT3Compaction(t, got, "summary", "e9", 42000)

		mu.Lock()
		defer mu.Unlock()
		if len(payloads) != 1 {
			t.Fatalf("onPayload calls = %d, want 1", len(payloads))
		}
		if errs[0] != nil {
			t.Fatalf("onPayload returned error: %v", errs[0])
		}
		var want any
		if err := json.Unmarshal(t3PlanPayload, &want); err != nil {
			t.Fatalf("unmarshal expected payload: %v", err)
		}
		if !reflect.DeepEqual(payloads[0], want) {
			t.Errorf("onPayload returned %#v, want %#v", payloads[0], want)
		}

		calls := h.stream.callsCopy()
		if len(calls) != 1 {
			t.Fatalf("Stream calls = %d, want 1", len(calls))
		}
		var wantRequest map[string]any
		if err := json.Unmarshal(t3PlanRequest, &wantRequest); err != nil {
			t.Fatalf("unmarshal expected request: %v", err)
		}
		if !reflect.DeepEqual(calls[0].request, wantRequest) {
			t.Errorf("Stream request = %#v, want %#v", calls[0].request, wantRequest)
		}
		if calls[0].model == nil {
			t.Error("Stream model is nil, want the resolved model map")
		}
	})

	t.Run("empty plan payload defers to host", func(t *testing.T) {
		var (
			mu       sync.Mutex
			payloads []any
		)
		h := newT3Harness(t,
			func(context.Context, PlanInput) (PlanOutput, error) {
				return PlanOutput{Action: "takeover", Request: t3PlanRequest}, nil
			},
			func(context.Context, ParseInput) (ParseOutput, error) {
				return ParseOutput{Summary: "summary"}, nil
			},
			func(_ int, _ map[string]any, _ map[string]any, onPayload func(any, map[string]any) (any, error)) (map[string]any, error) {
				got, err := onPayload(map[string]any{"host": "body"}, nil)
				if err != nil {
					return nil, err
				}
				mu.Lock()
				payloads = append(payloads, got)
				mu.Unlock()
				return t3SuccessResponse("summary"), nil
			},
		)

		got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
		assertT3Compaction(t, got, "summary", "e9", 42000)

		mu.Lock()
		defer mu.Unlock()
		if len(payloads) != 1 || payloads[0] != nil {
			t.Errorf("onPayload returned %#v, want nil for an empty plan payload", payloads)
		}
	})
}

func TestT3BeforeCompactRetriesParse(t *testing.T) {
	findModelCalls := 0
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) {
			return t3TakeoverPlan(), nil
		},
		func(_ context.Context, in ParseInput) (ParseOutput, error) {
			if in.Attempt == 0 {
				return ParseOutput{Retry: true, Why: "summary too short"}, nil
			}
			return ParseOutput{Summary: "second attempt summary"}, nil
		},
		func(int, map[string]any, map[string]any, func(any, map[string]any) (any, error)) (map[string]any, error) {
			return t3SuccessResponse("first"), nil
		},
	)
	h.driver.findModel = func(provider, modelID string) (map[string]any, bool) {
		findModelCalls++
		return map[string]any{"provider": provider, "id": modelID}, true
	}

	got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	assertT3Compaction(t, got, "second attempt summary", "e9", 42000)

	plans := h.plugin.plans()
	if len(plans) != 2 {
		t.Fatalf("Plan calls = %d, want 2", len(plans))
	}
	if plans[0].Attempt != 0 || plans[1].Attempt != 1 {
		t.Errorf("Plan attempts = %d/%d, want 0/1", plans[0].Attempt, plans[1].Attempt)
	}
	if n := h.stream.callCount(); n != 2 {
		t.Errorf("Stream calls = %d, want 2", n)
	}
	parses := h.plugin.parses()
	if len(parses) != 2 {
		t.Fatalf("Parse calls = %d, want 2", len(parses))
	}
	if parses[0].Attempt != 0 || parses[1].Attempt != 1 {
		t.Errorf("Parse attempts = %d/%d, want 0/1", parses[0].Attempt, parses[1].Attempt)
	}
	if findModelCalls != 1 {
		t.Errorf("FindModel calls = %d, want 1 (resolved once outside the retry loop)", findModelCalls)
	}
}

func TestT3BeforeCompactParseNoSummary(t *testing.T) {
	t.Run("none on first attempt", func(t *testing.T) {
		h := newT3Harness(t,
			func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
			func(context.Context, ParseInput) (ParseOutput, error) {
				return ParseOutput{None: true, Why: "no <summary> tag"}, nil
			},
			func(int, map[string]any, map[string]any, func(any, map[string]any) (any, error)) (map[string]any, error) {
				return t3SuccessResponse("not a summary"), nil
			},
		)

		got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
		if got != nil {
			t.Fatalf("BeforeCompact returned %#v, want nil", got)
		}
		if fails := h.state.Breaker(t3TestSessionID).Fails; fails != 1 {
			t.Errorf("Breaker.Fails = %d, want 1", fails)
		}
		recs := h.sink.records()
		if len(recs) != 1 || recs[0].Outcome != OutcomeFallback {
			t.Fatalf("trace = %#v, want one fallback record", recs)
		}
		if want := "no <summary> tag (text=not a summary)"; recs[0].Detail != want {
			t.Errorf("detail = %q, want %q", recs[0].Detail, want)
		}
	})

	t.Run("retry then none", func(t *testing.T) {
		h := newT3Harness(t,
			func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
			func(_ context.Context, in ParseInput) (ParseOutput, error) {
				if in.Attempt == 0 {
					return ParseOutput{Retry: true, Why: "try again"}, nil
				}
				return ParseOutput{None: true, Why: "still no summary"}, nil
			},
			func(int, map[string]any, map[string]any, func(any, map[string]any) (any, error)) (map[string]any, error) {
				return t3SuccessResponse("plain text"), nil
			},
		)

		got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
		if got != nil {
			t.Fatalf("BeforeCompact returned %#v, want nil", got)
		}
		if plans := h.plugin.plans(); len(plans) != 2 {
			t.Errorf("Plan calls = %d, want 2", len(plans))
		}
		if n := h.stream.callCount(); n != 2 {
			t.Errorf("Stream calls = %d, want 2", n)
		}
		if parses := h.plugin.parses(); len(parses) != 2 {
			t.Errorf("Parse calls = %d, want 2", len(parses))
		}
		if fails := h.state.Breaker(t3TestSessionID).Fails; fails != 1 {
			t.Errorf("Breaker.Fails = %d, want 1", fails)
		}
		recs := h.sink.records()
		if len(recs) != 2 {
			t.Fatalf("trace = %#v, want the rejected attempt and the fallback", recs)
		}
		if recs[0].Outcome != OutcomeRetry || recs[0].Detail != "try again" {
			t.Errorf("first record = %#v, want the retry reason", recs[0])
		}
		if recs[1].Outcome != OutcomeFallback {
			t.Fatalf("second record = %#v, want a fallback", recs[1])
		}
		if want := "still no summary (text=plain text)"; recs[1].Detail != want {
			t.Errorf("detail = %q, want %q", recs[1].Detail, want)
		}
	})
}

func TestT3BeforeCompactRetryExhausted(t *testing.T) {
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
		func(context.Context, ParseInput) (ParseOutput, error) {
			return ParseOutput{Retry: true, None: true, Why: "keep asking"}, nil
		},
		func(int, map[string]any, map[string]any, func(any, map[string]any) (any, error)) (map[string]any, error) {
			return t3SuccessResponse("plain text"), nil
		},
	)

	got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	if got != nil {
		t.Fatalf("BeforeCompact returned %#v, want nil", got)
	}
	if plans := h.plugin.plans(); len(plans) != 2 {
		t.Errorf("Plan calls = %d, want 2", len(plans))
	}
	if n := h.stream.callCount(); n != 2 {
		t.Errorf("Stream calls = %d, want 2", n)
	}
	if fails := h.state.Breaker(t3TestSessionID).Fails; fails != 1 {
		t.Errorf("Breaker.Fails = %d, want 1", fails)
	}
	recs := h.sink.records()
	if len(recs) != 3 {
		t.Fatalf("trace = %#v, want both rejected attempts and the fallback", recs)
	}
	for i, rec := range recs[:2] {
		if rec.Outcome != OutcomeRetry || rec.Detail != "keep asking" {
			t.Errorf("record %d = %#v, want one retry reason per attempt", i, rec)
		}
	}
	last := recs[2]
	if last.Outcome != OutcomeFallback {
		t.Fatalf("last record = %#v, want a fallback", last)
	}
	// The detail now carries the provider's own explanation, because a refused
	// resend explains itself only through the terminal message.
	if !strings.HasPrefix(last.Detail, "no summary after two attempts") {
		t.Errorf("detail = %q, want the two-attempt prefix", last.Detail)
	}
	for _, want := range []string{"stopReason=end_turn", "outputTokens=", "text=plain text"} {
		if !strings.Contains(last.Detail, want) {
			t.Errorf("detail = %q, want it to contain %q", last.Detail, want)
		}
	}
}

func TestT3BeforeCompactStreamRetries(t *testing.T) {
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
		func(context.Context, ParseInput) (ParseOutput, error) {
			return ParseOutput{Summary: "recovered"}, nil
		},
		func(call int, _ map[string]any, _ map[string]any, _ func(any, map[string]any) (any, error)) (map[string]any, error) {
			if call == 0 {
				return nil, errors.New("temporary stream failure")
			}
			return t3SuccessResponse("summary"), nil
		},
	)

	got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	assertT3Compaction(t, got, "recovered", "e9", 42000)

	if n := h.stream.callCount(); n != 2 {
		t.Errorf("Stream calls = %d, want 2", n)
	}
	delays := h.sleep.calls()
	if len(delays) < 1 {
		t.Fatalf("Sleep calls = %d, want at least 1", len(delays))
	}
	if delays[0] != 250*time.Millisecond {
		t.Errorf("first Sleep delay = %v, want 250ms", delays[0])
	}
	recs := h.sink.records()
	if len(recs) != 1 || recs[0].Outcome != OutcomeCompacted {
		t.Fatalf("trace = %#v, want compacted", recs)
	}
}

func TestT3BeforeCompactStreamFailureTwice(t *testing.T) {
	streamErr := errors.New("provider unavailable")
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
		func(context.Context, ParseInput) (ParseOutput, error) {
			return ParseOutput{Summary: "should not be reached"}, nil
		},
		func(int, map[string]any, map[string]any, func(any, map[string]any) (any, error)) (map[string]any, error) {
			return nil, streamErr
		},
	)

	got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	if got != nil {
		t.Fatalf("BeforeCompact returned %#v, want nil", got)
	}
	if n := h.stream.callCount(); n != 2 {
		t.Errorf("Stream calls = %d, want 2", n)
	}
	if got := len(h.sleep.calls()); got != 1 {
		t.Errorf("Sleep calls = %d, want 1", got)
	}
	if parses := h.plugin.parses(); len(parses) != 0 {
		t.Errorf("Parse calls = %d, want 0", len(parses))
	}
	if fails := h.state.Breaker(t3TestSessionID).Fails; fails != 1 {
		t.Errorf("Breaker.Fails = %d, want 1", fails)
	}
	recs := h.sink.records()
	if len(recs) != 1 || recs[0].Outcome != OutcomeFailed {
		t.Fatalf("trace = %#v, want one failed record", recs)
	}
	if !strings.Contains(recs[0].Detail, streamErr.Error()) {
		t.Errorf("detail = %q, want it to carry %q", recs[0].Detail, streamErr.Error())
	}
}

func TestT3BeforeCompactFindModelMissing(t *testing.T) {
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
		func(context.Context, ParseInput) (ParseOutput, error) {
			return ParseOutput{Summary: "unused"}, nil
		},
		func(int, map[string]any, map[string]any, func(any, map[string]any) (any, error)) (map[string]any, error) {
			return t3SuccessResponse("unused"), nil
		},
	)
	h.driver.findModel = func(string, string) (map[string]any, bool) {
		return nil, false
	}

	got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	if got != nil {
		t.Fatalf("BeforeCompact returned %#v, want nil", got)
	}
	if n := h.stream.callCount(); n != 0 {
		t.Errorf("Stream calls = %d, want 0", n)
	}
	if fails := h.state.Breaker(t3TestSessionID).Fails; fails != 1 {
		t.Errorf("Breaker.Fails = %d, want 1", fails)
	}
	recs := h.sink.records()
	if len(recs) != 1 || recs[0].Outcome != OutcomeFailed {
		t.Fatalf("trace = %#v, want one failed record", recs)
	}
}

func TestT3BeforeCompactPlanError(t *testing.T) {
	planErr := errors.New("moe hook crashed")
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) {
			return PlanOutput{}, planErr
		},
		nil,
		nil,
	)

	got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	if got != nil {
		t.Fatalf("BeforeCompact returned %#v, want nil", got)
	}
	if n := h.stream.callCount(); n != 0 {
		t.Errorf("Stream calls = %d, want 0", n)
	}
	if fails := h.state.Breaker(t3TestSessionID).Fails; fails != 1 {
		t.Errorf("Breaker.Fails = %d, want 1", fails)
	}
	recs := h.sink.records()
	if len(recs) != 1 || recs[0].Outcome != OutcomeFailed {
		t.Fatalf("trace = %#v, want one failed record", recs)
	}
	if !strings.Contains(recs[0].Detail, planErr.Error()) {
		t.Errorf("detail = %q, want it to carry %q", recs[0].Detail, planErr.Error())
	}
}

func TestT3BeforeCompactParseError(t *testing.T) {
	parseErr := errors.New("parse hook crashed")
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
		func(context.Context, ParseInput) (ParseOutput, error) {
			return ParseOutput{}, parseErr
		},
		func(int, map[string]any, map[string]any, func(any, map[string]any) (any, error)) (map[string]any, error) {
			return t3SuccessResponse("text"), nil
		},
	)

	got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	if got != nil {
		t.Fatalf("BeforeCompact returned %#v, want nil", got)
	}
	if fails := h.state.Breaker(t3TestSessionID).Fails; fails != 1 {
		t.Errorf("Breaker.Fails = %d, want 1", fails)
	}
	recs := h.sink.records()
	if len(recs) != 1 || recs[0].Outcome != OutcomeFailed {
		t.Fatalf("trace = %#v, want one failed record", recs)
	}
	if !strings.Contains(recs[0].Detail, parseErr.Error()) {
		t.Errorf("detail = %q, want it to carry %q", recs[0].Detail, parseErr.Error())
	}
}

func TestT3BeforeCompactContextCancelledDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
		func(context.Context, ParseInput) (ParseOutput, error) {
			return ParseOutput{Summary: "unused"}, nil
		},
		func(int, map[string]any, map[string]any, func(any, map[string]any) (any, error)) (map[string]any, error) {
			<-release
			return t3SuccessResponse("unused"), nil
		},
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan map[string]any, 1)
	go func() {
		done <- h.driver.BeforeCompact(ctx, t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	}()

	select {
	case got := <-done:
		if got != nil {
			t.Fatalf("BeforeCompact returned %#v, want nil", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("BeforeCompact blocked despite an already-cancelled context")
	}

	if n := h.stream.callCount(); n != 0 {
		t.Errorf("Stream calls = %d, want 0 when ctx is already cancelled", n)
	}
	if fails := h.state.Breaker(t3TestSessionID).Fails; fails != 1 {
		t.Errorf("Breaker.Fails = %d, want 1", fails)
	}
}

func TestT3BeforeCompactTextBlocksAndUsage(t *testing.T) {
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
		func(context.Context, ParseInput) (ParseOutput, error) {
			return ParseOutput{Summary: "summary"}, nil
		},
		func(int, map[string]any, map[string]any, func(any, map[string]any) (any, error)) (map[string]any, error) {
			return map[string]any{
				"content": []any{
					map[string]any{"type": "text", "text": "a"},
					map[string]any{"type": "toolCall", "id": "tool-1"},
					map[string]any{"type": "text", "text": "b"},
					map[string]any{"type": "text", "text": float64(7)},
					"not a block",
				},
				"stopReason": "max_tokens",
				"usage": map[string]any{
					"input":     json.Number("9000"),
					"cacheRead": 8192,
					"output":    int64(640),
				},
			}, nil
		},
	)

	got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	assertT3Compaction(t, got, "summary", "e9", 42000)

	parses := h.plugin.parses()
	if len(parses) != 1 {
		t.Fatalf("Parse calls = %d, want 1", len(parses))
	}
	if parses[0].Text != "a\nb" {
		t.Errorf("ParseInput.Text = %q, want %q", parses[0].Text, "a\nb")
	}
	if strings.Contains(parses[0].Text, "toolCall") {
		t.Errorf("ParseInput.Text contains a tool call: %q", parses[0].Text)
	}
	if parses[0].StopReason != "max_tokens" {
		t.Errorf("ParseInput.StopReason = %q, want max_tokens", parses[0].StopReason)
	}

	recs := h.sink.records()
	if len(recs) != 1 {
		t.Fatalf("trace records = %d, want 1", len(recs))
	}
	if recs[0].InputTokens != 9000 || recs[0].CacheReadTokens != 8192 || recs[0].OutputTokens != 640 {
		t.Errorf("trace tokens = %d/%d/%d, want 9000/8192/640", recs[0].InputTokens, recs[0].CacheReadTokens, recs[0].OutputTokens)
	}
}

func TestT3TraceAppendErrorIsIgnored(t *testing.T) {
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
		func(context.Context, ParseInput) (ParseOutput, error) {
			return ParseOutput{Summary: "summary"}, nil
		},
		func(int, map[string]any, map[string]any, func(any, map[string]any) (any, error)) (map[string]any, error) {
			return t3SuccessResponse("summary"), nil
		},
	)
	h.sink.err = errors.New("trace disk full")

	got := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	assertT3Compaction(t, got, "summary", "e9", 42000)
	if recs := h.sink.records(); len(recs) != 1 {
		t.Errorf("trace records = %d, want the append to be attempted once", len(recs))
	}
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func TestT3UsageInt(t *testing.T) {
	cases := []struct {
		name string
		res  map[string]any
		key  string
		want int
	}{
		{name: "float64", res: map[string]any{"usage": map[string]any{"input": float64(12)}}, key: "input", want: 12},
		{name: "int", res: map[string]any{"usage": map[string]any{"input": 34}}, key: "input", want: 34},
		{name: "json number integer", res: map[string]any{"usage": map[string]any{"input": json.Number("56")}}, key: "input", want: 56},
		{name: "json number float", res: map[string]any{"usage": map[string]any{"input": json.Number("78.9")}}, key: "input", want: 78},
		{name: "missing key", res: map[string]any{"usage": map[string]any{"input": float64(1)}}, key: "output", want: 0},
		{name: "missing usage", res: map[string]any{"stopReason": "end_turn"}, key: "input", want: 0},
		{name: "wrong usage type", res: map[string]any{"usage": "nope"}, key: "input", want: 0},
		{name: "wrong value type", res: map[string]any{"usage": map[string]any{"input": "12"}}, key: "input", want: 0},
		{name: "nil result", res: nil, key: "input", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageInt(tc.res, tc.key); got != tc.want {
				t.Errorf("usageInt() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestT3TextBlocks(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  string
	}{
		{name: "nil", input: nil, want: ""},
		{name: "wrong container", input: "text", want: ""},
		{name: "empty", input: []any{}, want: ""},
		{name: "skips non text", input: []any{
			"nope",
			map[string]any{"type": "toolCall", "text": "ignored"},
			map[string]any{"type": "text"},
			map[string]any{"type": "text", "text": 42},
			map[string]any{"type": "text", "text": "kept"},
		}, want: "kept"},
		{name: "joins with newline", input: []any{
			map[string]any{"type": "text", "text": "a"},
			map[string]any{"type": "text", "text": "b"},
		}, want: "a\nb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := textBlocks(tc.input); got != tc.want {
				t.Errorf("textBlocks() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestT3StopReason(t *testing.T) {
	cases := []struct {
		name string
		res  map[string]any
		want string
	}{
		{name: "present", res: map[string]any{"stopReason": "end_turn"}, want: "end_turn"},
		{name: "missing", res: map[string]any{}, want: ""},
		{name: "wrong type", res: map[string]any{"stopReason": 1}, want: ""},
		{name: "nil", res: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stopReason(tc.res); got != tc.want {
				t.Errorf("stopReason() = %q, want %q", got, tc.want)
			}
		})
	}
}

// t3SplitTurnEvent is t3CompactEvent with the start of the turn the cut point
// falls into: Pi keeps those messages verbatim, so it asks for a checkpoint of
// them to put in front.
func t3SplitTurnEvent() CompactEvent {
	ev := t3CompactEvent()
	ev.Preparation.TurnPrefixMessages = []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"fix the flaky test"}]}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"looking at the runner"}]}`),
	}
	return ev
}

// t3CapturedBody is the session request the state holds: a system prompt, tools
// and one message, which is what a prefix request has to edit.
var t3CapturedBody = json.RawMessage(`{"model":"deepseek","system":"sys","tools":[{"name":"read"}],"messages":[{"role":"user","content":"hi"}]}`)

// TestBeforeCompactAppendsTurnContext: a cut that is not a turn start gets Pi's
// marker and a checkpoint, in Pi's order, and the checkpoint reply is not fed to
// the summary parser.
func TestBeforeCompactAppendsTurnContext(t *testing.T) {
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
		func(context.Context, ParseInput) (ParseOutput, error) {
			return ParseOutput{Summary: validSummary}, nil
		},
		func(call int, _ map[string]any, _ map[string]any, _ func(any, map[string]any) (any, error)) (map[string]any, error) {
			if call == 0 {
				return t3SuccessResponse(validSummary), nil
			}
			return t3SuccessResponse("## Original Request\nfix the flaky test"), nil
		},
	)
	h.state.Put(t3TestSessionID, t3TestModelID, t3TestProvider, t3CapturedBody)

	res := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3SplitTurnEvent())
	if res == nil {
		t.Fatal("BeforeCompact returned nil, want the takeover result")
	}
	compaction, ok := res["compaction"].(map[string]any)
	if !ok {
		t.Fatalf("result = %#v, want a compaction", res)
	}
	want := validSummary + "\n\n---\n\n**Turn Context (split turn):**\n\n## Original Request\nfix the flaky test"
	if got := compaction["summary"]; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if n := h.stream.callCount(); n != 2 {
		t.Fatalf("Stream calls = %d, want the summary and the checkpoint", n)
	}
	if parses := h.plugin.parses(); len(parses) != 1 {
		t.Fatalf("Parse calls = %d, want 1: the checkpoint is not a summary", len(parses))
	}

	// The checkpoint request starts from the captured body, so the provider
	// still sees the prefix it cached, and carries no tools.
	body, err := h.stream.callsCopy()[1].onPayload(nil, nil)
	if err != nil {
		t.Fatalf("onPayload: %v", err)
	}
	decoded, ok := body.(map[string]any)
	if !ok {
		t.Fatalf("prefix payload = %#v, want an object", body)
	}
	if decoded["system"] != "sys" {
		t.Fatalf("prefix payload lost the captured system prompt: %#v", decoded)
	}
	if _, kept := decoded["tools"]; kept {
		t.Fatalf("prefix payload kept the tools: %#v", decoded)
	}
	messages, ok := decoded["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("prefix messages = %#v, want the prompt alone", decoded["messages"])
	}

	recs := h.sink.records()
	if len(recs) != 1 || recs[0].Outcome != OutcomeCompacted || recs[0].Detail != "" {
		t.Fatalf("trace = %#v, want one clean compacted record", recs)
	}
	if fails := h.state.Breaker(t3TestSessionID).Fails; fails != 0 {
		t.Errorf("Breaker.Fails = %d, want 0", fails)
	}
}

// TestBeforeCompactSkipsTheTurnContextOnFailure: the checkpoint is an
// annotation, so a failed or unfinished reply drops it and keeps the summary
// rather than handing the whole compaction back.
func TestBeforeCompactSkipsTheTurnContextOnFailure(t *testing.T) {
	cases := []struct {
		name      string
		prefixRes map[string]any
		prefixErr error
		want      string
	}{
		{
			name:      "stream fails",
			prefixErr: errors.New("provider said no"),
			want:      "split turn: the prefix request failed (provider said no)",
		},
		{
			name:      "reply is empty",
			prefixRes: t3SuccessResponse("  "),
			want:      "split turn: the prefix reply was empty (stop=end_turn)",
		},
		{
			name: "reply was cut off",
			prefixRes: map[string]any{
				"content":    []any{map[string]any{"type": "text", "text": "## Original Request\nfix the fl"}},
				"stopReason": "length",
			},
			want: "split turn: the prefix reply did not finish (stop=length)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newT3Harness(t,
				func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
				func(context.Context, ParseInput) (ParseOutput, error) {
					return ParseOutput{Summary: validSummary}, nil
				},
				func(call int, _ map[string]any, _ map[string]any, _ func(any, map[string]any) (any, error)) (map[string]any, error) {
					// The first call is the summary; every later one is the
					// checkpoint, including the retry of a failed checkpoint.
					if call == 0 {
						return t3SuccessResponse(validSummary), nil
					}
					return tc.prefixRes, tc.prefixErr
				},
			)
			h.state.Put(t3TestSessionID, t3TestModelID, t3TestProvider, t3CapturedBody)

			res := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3SplitTurnEvent())
			if res == nil {
				t.Fatal("BeforeCompact returned nil, want the summary alone")
			}
			compaction, _ := res["compaction"].(map[string]any)
			if got := compaction["summary"]; got != validSummary {
				t.Fatalf("summary = %q, want the summary without a marker", got)
			}
			recs := h.sink.records()
			if len(recs) != 1 || recs[0].Outcome != OutcomeCompacted {
				t.Fatalf("trace = %#v, want one compacted record", recs)
			}
			if recs[0].Detail != tc.want {
				t.Fatalf("detail = %q, want %q", recs[0].Detail, tc.want)
			}
			if fails := h.state.Breaker(t3TestSessionID).Fails; fails != 0 {
				t.Errorf("Breaker.Fails = %d, want 0: a skipped checkpoint is not a failure", fails)
			}
		})
	}
}

// TestBeforeCompactWithoutATurnPrefixSkipsTheCall: Pi only asks for a checkpoint
// when the preparation carries one, so a split turn without messages adds
// nothing and costs nothing.
func TestBeforeCompactWithoutATurnPrefixSkipsTheCall(t *testing.T) {
	h := newT3Harness(t,
		func(context.Context, PlanInput) (PlanOutput, error) { return t3TakeoverPlan(), nil },
		func(context.Context, ParseInput) (ParseOutput, error) {
			return ParseOutput{Summary: validSummary}, nil
		},
		func(int, map[string]any, map[string]any, func(any, map[string]any) (any, error)) (map[string]any, error) {
			return t3SuccessResponse(validSummary), nil
		},
	)
	h.state.Put(t3TestSessionID, t3TestModelID, t3TestProvider, t3CapturedBody)

	res := h.driver.BeforeCompact(context.Background(), t3TestSessionID, t3TestModelID, t3TestProvider, t3CompactEvent())
	if res == nil {
		t.Fatal("BeforeCompact returned nil")
	}
	if n := h.stream.callCount(); n != 1 {
		t.Fatalf("Stream calls = %d, want 1", n)
	}
	compaction, _ := res["compaction"].(map[string]any)
	if got := compaction["summary"]; got != validSummary {
		t.Fatalf("summary = %q, want the summary alone", got)
	}
}
