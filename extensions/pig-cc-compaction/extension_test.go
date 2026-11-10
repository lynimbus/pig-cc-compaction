package pigcc

// The host bridges themselves cannot be tested against a real PiG: an
// sdk.Context only comes into existence inside a running host. Everything this
// file covers is what extension.go settles without one — the wire decoding, the
// numeric coercion, the trace path, the declarations the factory makes, and the
// handlers, driven through the hostCall seam instead of a faked sdk.Context.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// compactData is the session_before_compact payload PiG marshals, decoded the
// way encoding/json decodes it: numbers as float64, arrays as []any.
func compactData() map[string]any {
	return map[string]any{
		"type":               "session_before_compact",
		"reason":             "manual",
		"customInstructions": "",
		"willRetry":          false,
		"preparation": map[string]any{
			"firstKeptEntryId": "e9",
			"tokensBefore":     42000.0,
			"isSplitTurn":      false,
			"fileOps": map[string]any{
				"read":    []any{"a"},
				"written": []any{},
				"edited":  []any{},
			},
			"settings": map[string]any{
				"enabled":          true,
				"reserveTokens":    16384,
				"keepRecentTokens": 20000,
			},
		},
	}
}

// --- decodeCompactEvent ------------------------------------------------------

func TestDecodeCompactEventFloatTokens(t *testing.T) {
	ev, ok := decodeCompactEvent(compactData())
	if !ok {
		t.Fatal("decodeCompactEvent = false, want true")
	}
	if ev.Preparation.TokensBefore != 42000 {
		t.Fatalf("TokensBefore = %d, want 42000", ev.Preparation.TokensBefore)
	}
	if ev.Preparation.FirstKeptEntryID != "e9" {
		t.Fatalf("FirstKeptEntryID = %q, want %q", ev.Preparation.FirstKeptEntryID, "e9")
	}
	if ev.Reason != "manual" {
		t.Fatalf("Reason = %q, want %q", ev.Reason, "manual")
	}
	if ev.CustomInstructions != "" {
		t.Fatalf("CustomInstructions = %q, want empty", ev.CustomInstructions)
	}
	if ev.WillRetry {
		t.Fatal("WillRetry = true, want false")
	}
	if ev.Preparation.IsSplitTurn {
		t.Fatal("IsSplitTurn = true, want false")
	}
	if ev.Preparation.PreviousSummary != "" {
		t.Fatalf("PreviousSummary = %q, want empty", ev.Preparation.PreviousSummary)
	}
	if !ev.Preparation.Settings.Enabled {
		t.Fatal("Settings.Enabled = false, want true")
	}
	if ev.Preparation.Settings.ReserveTokens != 16384 {
		t.Fatalf("Settings.ReserveTokens = %d, want 16384", ev.Preparation.Settings.ReserveTokens)
	}
	if ev.Preparation.Settings.KeepRecentTokens != 20000 {
		t.Fatalf("Settings.KeepRecentTokens = %d, want 20000", ev.Preparation.Settings.KeepRecentTokens)
	}
	if !slices.Equal(ev.Preparation.FileOps.Read, []string{"a"}) {
		t.Fatalf("FileOps.Read = %#v, want [a]", ev.Preparation.FileOps.Read)
	}
	if len(ev.Preparation.FileOps.Written) != 0 || len(ev.Preparation.FileOps.Edited) != 0 {
		t.Fatalf("FileOps = %#v, want empty written and edited",
			ev.Preparation.FileOps)
	}
}

func TestDecodeCompactEventJSONNumberTokens(t *testing.T) {
	data := compactData()
	prep := data["preparation"].(map[string]any)
	prep["tokensBefore"] = json.Number("42000")
	settings := prep["settings"].(map[string]any)
	settings["reserveTokens"] = json.Number("16384")
	settings["keepRecentTokens"] = json.Number("20000")

	ev, ok := decodeCompactEvent(data)
	if !ok {
		t.Fatal("decodeCompactEvent = false, want true")
	}
	if ev.Preparation.TokensBefore != 42000 {
		t.Fatalf("TokensBefore = %d, want 42000", ev.Preparation.TokensBefore)
	}
	if ev.Preparation.FirstKeptEntryID != "e9" {
		t.Fatalf("FirstKeptEntryID = %q, want %q", ev.Preparation.FirstKeptEntryID, "e9")
	}
	if ev.Preparation.Settings.ReserveTokens != 16384 {
		t.Fatalf("Settings.ReserveTokens = %d, want 16384", ev.Preparation.Settings.ReserveTokens)
	}
	if ev.Preparation.Settings.KeepRecentTokens != 20000 {
		t.Fatalf("Settings.KeepRecentTokens = %d, want 20000", ev.Preparation.Settings.KeepRecentTokens)
	}
}

// TestDecodeCompactEventFromJSONText pins the real wire shape: the host sends
// the event as JSON, so the decoder sees exactly what encoding/json produces —
// float64 by default, json.Number when the decoder keeps the source text.
func TestDecodeCompactEventFromJSONText(t *testing.T) {
	const wire = `{
		"type": "session_before_compact",
		"reason": "manual",
		"customInstructions": "",
		"willRetry": false,
		"preparation": {
			"firstKeptEntryId": "e9",
			"tokensBefore": 42000,
			"previousSummary": "earlier work",
			"isSplitTurn": false,
			"fileOps": {"read": ["a"], "written": [], "edited": []},
			"settings": {"enabled": true, "reserveTokens": 16384, "keepRecentTokens": 20000}
		}
	}`

	t.Run("float64", func(t *testing.T) {
		var data map[string]any
		if err := json.Unmarshal([]byte(wire), &data); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		ev, ok := decodeCompactEvent(data)
		if !ok {
			t.Fatal("decodeCompactEvent = false, want true")
		}
		if ev.Preparation.TokensBefore != 42000 {
			t.Fatalf("TokensBefore = %d, want 42000", ev.Preparation.TokensBefore)
		}
		if ev.Preparation.PreviousSummary != "earlier work" {
			t.Fatalf("PreviousSummary = %q, want %q",
				ev.Preparation.PreviousSummary, "earlier work")
		}
	})

	t.Run("json.Number", func(t *testing.T) {
		dec := json.NewDecoder(strings.NewReader(wire))
		dec.UseNumber()
		var data map[string]any
		if err := dec.Decode(&data); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ev, ok := decodeCompactEvent(data)
		if !ok {
			t.Fatal("decodeCompactEvent = false, want true")
		}
		if ev.Preparation.TokensBefore != 42000 {
			t.Fatalf("TokensBefore = %d, want 42000", ev.Preparation.TokensBefore)
		}
	})
}

func TestDecodeCompactEventWithoutPreparation(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
	}{
		{"missing key", map[string]any{"reason": "manual", "willRetry": false}},
		{"empty map", map[string]any{}},
		{"nil data", nil},
		{"nil preparation", map[string]any{"preparation": nil}},
		{"preparation is a string", map[string]any{"preparation": "e9"}},
		{"preparation is an array", map[string]any{"preparation": []any{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := decodeCompactEvent(tc.data)
			if ok {
				t.Fatalf("decodeCompactEvent = %#v, true; want false", ev)
			}
		})
	}
}

// TestDecodeCompactEventSparsePreparation keeps a thin preparation usable: a
// missing fileOps or settings block must not lose the compaction.
func TestDecodeCompactEventSparsePreparation(t *testing.T) {
	data := map[string]any{
		"reason":      "threshold",
		"willRetry":   true,
		"preparation": map[string]any{"tokensBefore": 42000},
	}
	ev, ok := decodeCompactEvent(data)
	if !ok {
		t.Fatal("decodeCompactEvent = false, want true")
	}
	if !ev.WillRetry {
		t.Fatal("WillRetry = false, want true")
	}
	if ev.Preparation.TokensBefore != 42000 {
		t.Fatalf("TokensBefore = %d, want 42000", ev.Preparation.TokensBefore)
	}
	if ev.Preparation.Settings.Enabled {
		t.Fatal("Settings.Enabled = true, want false")
	}
	if ev.Preparation.Settings.ReserveTokens != 0 || ev.Preparation.Settings.KeepRecentTokens != 0 {
		t.Fatalf("Settings = %#v, want zero values", ev.Preparation.Settings)
	}
	if ev.Preparation.FileOps.Read != nil || ev.Preparation.FileOps.Written != nil ||
		ev.Preparation.FileOps.Edited != nil {
		t.Fatalf("FileOps = %#v, want nil lists", ev.Preparation.FileOps)
	}
}

// TestDecodeCompactEventIgnoresForeignShapes keeps a malformed optional field
// from failing the whole event: only preparation decides whether to take over.
func TestDecodeCompactEventIgnoresForeignShapes(t *testing.T) {
	data := map[string]any{
		"reason":             42,
		"customInstructions": []any{"be", "brief"},
		"willRetry":          "yes",
		"preparation": map[string]any{
			"firstKeptEntryId": 7,
			"tokensBefore":     "42000",
			"isSplitTurn":      1,
			"fileOps":          "none",
			"settings":         []any{true},
		},
	}
	ev, ok := decodeCompactEvent(data)
	if !ok {
		t.Fatal("decodeCompactEvent = false, want true")
	}
	if ev.Reason != "" || ev.CustomInstructions != "" || ev.WillRetry {
		t.Fatalf("foreign scalars decoded: %#v", ev)
	}
	if ev.Preparation.FirstKeptEntryID != "" {
		t.Fatalf("FirstKeptEntryID = %q, want empty", ev.Preparation.FirstKeptEntryID)
	}
	if ev.Preparation.TokensBefore != 0 {
		t.Fatalf("TokensBefore = %d, want 0", ev.Preparation.TokensBefore)
	}
	if ev.Preparation.IsSplitTurn {
		t.Fatal("IsSplitTurn = true, want false")
	}
	if !reflect.DeepEqual(ev.Preparation.FileOps, FileOps{}) {
		t.Fatalf("FileOps = %#v, want zero", ev.Preparation.FileOps)
	}
	if !reflect.DeepEqual(ev.Preparation.Settings, Settings{}) {
		t.Fatalf("Settings = %#v, want zero", ev.Preparation.Settings)
	}
}

// TestDecodeCompactEventFileOps checks every list is decoded, not just read.
func TestDecodeCompactEventFileOps(t *testing.T) {
	data := map[string]any{
		"preparation": map[string]any{
			"tokensBefore": 1.0,
			"fileOps": map[string]any{
				"read":    []any{"a.go", "b.go"},
				"written": []any{"c.go"},
				"edited":  []any{"d.go", "e.go", "f.go"},
			},
		},
	}
	ev, ok := decodeCompactEvent(data)
	if !ok {
		t.Fatal("decodeCompactEvent = false, want true")
	}
	if !slices.Equal(ev.Preparation.FileOps.Read, []string{"a.go", "b.go"}) {
		t.Fatalf("Read = %#v", ev.Preparation.FileOps.Read)
	}
	if !slices.Equal(ev.Preparation.FileOps.Written, []string{"c.go"}) {
		t.Fatalf("Written = %#v", ev.Preparation.FileOps.Written)
	}
	if !slices.Equal(ev.Preparation.FileOps.Edited, []string{"d.go", "e.go", "f.go"}) {
		t.Fatalf("Edited = %#v", ev.Preparation.FileOps.Edited)
	}
}

// --- decodePayload -----------------------------------------------------------

func TestDecodePayload(t *testing.T) {
	body := map[string]any{"model": "group/deepseek", "messages": []any{}}
	data := map[string]any{"type": "before_provider_request", "payload": body}

	got, ok := decodePayload(data)
	if !ok {
		t.Fatal("decodePayload = false, want true")
	}
	gotMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("decodePayload value is %T, want map[string]any", got)
	}
	if !reflect.DeepEqual(gotMap, body) {
		t.Fatalf("payload = %#v, want %#v", gotMap, body)
	}
}

// TestDecodePayloadReturnsValueVerbatim keeps the captured body byte-identical:
// a raw JSON payload must come back as the same bytes, not a re-encoding.
func TestDecodePayloadReturnsValueVerbatim(t *testing.T) {
	raw := json.RawMessage(`{"model":"group/deepseek","messages":[]}`)
	got, ok := decodePayload(map[string]any{"payload": raw})
	if !ok {
		t.Fatal("decodePayload = false, want true")
	}
	gotRaw, ok := got.(json.RawMessage)
	if !ok {
		t.Fatalf("decodePayload value is %T, want json.RawMessage", got)
	}
	if len(gotRaw) != len(raw) || &gotRaw[0] != &raw[0] {
		t.Fatalf("payload was copied: got %s, want %s", gotRaw, raw)
	}
}

func TestDecodePayloadMissing(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
	}{
		{"empty map", map[string]any{}},
		{"other keys", map[string]any{"type": "before_provider_request"}},
		{"nil payload", map[string]any{"payload": nil}},
		{"nil data", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := decodePayload(tc.data); ok {
				t.Fatalf("decodePayload = %#v, true; want false", got)
			}
		})
	}
}

// --- num ---------------------------------------------------------------------

func TestNum(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"float64", 42000.0, 42000},
		{"float64 truncates", 42000.9, 42000},
		{"float64 negative", -2.5, -2},
		{"float32", float32(12.75), 12},
		{"json.Number integer", json.Number("42000"), 42000},
		{"json.Number fraction", json.Number("42000.8"), 42000},
		{"json.Number garbage", json.Number("nope"), 0},
		{"int", 7, 7},
		{"int64", int64(-3), -3},
		{"uint64", uint64(9), 9},
		{"nil", nil, 0},
		{"string", "42000", 0},
		{"bool", true, 0},
		{"slice", []any{1}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := num(tc.in); got != tc.want {
				t.Fatalf("num(%#v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// --- tracePath ---------------------------------------------------------------

func TestTracePathFromEnv(t *testing.T) {
	want := filepath.Join(t.TempDir(), "x.jsonl")
	t.Setenv(traceEnv, want)
	if got := tracePath(); got != want {
		t.Fatalf("tracePath() = %q, want %q", got, want)
	}
}

func TestTracePathDefault(t *testing.T) {
	t.Setenv(traceEnv, "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := tracePath()
	want := filepath.Join(home, ".pi", "agent", "pi-cc-trace.jsonl")
	if got != want {
		t.Fatalf("tracePath() = %q, want %q", got, want)
	}
	if !strings.HasSuffix(got, filepath.Join(".pi", "agent", "pi-cc-trace.jsonl")) {
		t.Fatalf("tracePath() = %q, want suffix %q", got,
			filepath.Join(".pi", "agent", "pi-cc-trace.jsonl"))
	}
}

// --- wiring ------------------------------------------------------------------

// TestExtensionRegistersCompactionWiring is the only place the registrations can
// be seen: the sdk keeps its declarations private, so the endpoint keeps the
// ledger of what it handed the host. The event names are written out as the
// wire names on purpose — a rename on either side must break this test.
func TestExtensionRegistersCompactionWiring(t *testing.T) {
	t.Setenv(pluginEnv, "")
	t.Setenv(traceEnv, "")

	e, ep := newExtension()
	if e == nil {
		t.Fatal("newExtension() = nil")
	}
	if got := e.Name(); got != "pig-cc-compaction" {
		t.Fatalf("Name() = %q, want %q", got, "pig-cc-compaction")
	}
	want := []string{
		"event:before_provider_request",
		"event:session_before_compact",
		"event:session_shutdown",
		"command:cc-compact",
	}
	if got := ep.declarations(); !slices.Equal(got, want) {
		t.Fatalf("declarations() = %#v, want %#v", got, want)
	}
	if ep.loadErr != nil {
		t.Fatalf("loadErr = %v, want nil", ep.loadErr)
	}
}

// TestExtensionSurvivesPluginLoadFailure pins the startup contract: a broken
// PIG_CC_PLUGIN is reported at session start, never panicked at load.
func TestExtensionSurvivesPluginLoadFailure(t *testing.T) {
	t.Setenv(pluginEnv, filepath.Join(t.TempDir(), "does-not-exist.js"))
	t.Setenv(traceEnv, "")

	e, ep := newExtension()
	if e == nil {
		t.Fatal("newExtension() = nil")
	}
	if got := e.Name(); got != "pig-cc-compaction" {
		t.Fatalf("Name() = %q, want %q", got, "pig-cc-compaction")
	}
	if ep.loadErr == nil {
		t.Fatal("loadErr = nil, want the read error")
	}
	want := []string{"event:session_start", "command:cc-compact"}
	if got := ep.declarations(); !slices.Equal(got, want) {
		t.Fatalf("declarations() = %#v, want %#v", got, want)
	}
}

// TestExtensionLoadsPluginFromEnvFile covers the reason PIG_CC_PLUGIN exists: a
// policy file on disk replaces the embedded one without a rebuild, and a policy
// that loads keeps the whole compaction path registered.
func TestExtensionLoadsPluginFromEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.js")
	if err := os.WriteFile(path, []byte(DefaultSource()), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	t.Setenv(pluginEnv, path)
	t.Setenv(traceEnv, "")

	e, ep := newExtension()
	if e == nil {
		t.Fatal("newExtension() = nil")
	}
	if ep.loadErr != nil {
		t.Fatalf("loadErr = %v, want nil", ep.loadErr)
	}
	want := []string{
		"event:before_provider_request",
		"event:session_before_compact",
		"event:session_shutdown",
		"command:cc-compact",
	}
	if got := ep.declarations(); !slices.Equal(got, want) {
		t.Fatalf("declarations() = %#v, want %#v", got, want)
	}
}

// TestExtensionFactoryNeverPanics calls the factory the way PiG does, so the
// exported entry point is covered too, not just the wiring behind it.
func TestExtensionFactoryNeverPanics(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"embedded plugin", ""},
		{"missing plugin file", filepath.Join(t.TempDir(), "nope.js")},
		{"spec path", "/nonexistent/plugin.js"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(pluginEnv, tc.src)
			t.Setenv(traceEnv, "")
			e := Extension()
			if e == nil {
				t.Fatal("Extension() = nil")
			}
			if got := e.Name(); got != "pig-cc-compaction" {
				t.Fatalf("Name() = %q, want %q", got, "pig-cc-compaction")
			}
		})
	}
}

// --- host fakes --------------------------------------------------------------
//
// sdk.Context cannot be constructed outside a host, so the endpoint talks to a
// hostCall for the length of one handler. These fakes implement that seam; the
// sdk adapter itself (sdkHost) is exercised only by a real PiG.

// stubPlugin keeps moejs out of the handler tests: the driver is faked there, so
// the policy never runs.
type stubPlugin struct{}

func (stubPlugin) Plan(context.Context, PlanInput) (PlanOutput, error) { return PlanOutput{}, nil }
func (stubPlugin) Parse(context.Context, ParseInput) (ParseOutput, error) {
	return ParseOutput{}, nil
}

// recordingSink is the real trace sink as the tests see it.
type recordingSink struct {
	records []TraceRecord
}

func (s *recordingSink) Append(rec TraceRecord) error {
	s.records = append(s.records, rec)
	return nil
}

type notifyCall struct {
	message string
	level   string
}

// fakeHost is one handler's sdk.Context, recorded.
type fakeHost struct {
	sessionID  string
	sessionErr error
	modelID    string
	providerID string
	signal     context.Context

	notifies    []notifyCall
	compacts    []map[string]any
	finds       int
	streams     int
	settings    map[string]any
	settingsErr error
	usage       usageView
	usageErr    error
}

var _ hostCall = (*fakeHost)(nil)

func (h *fakeHost) SessionID() (string, error) { return h.sessionID, h.sessionErr }
func (h *fakeHost) Model() string              { return h.modelID }
func (h *fakeHost) ModelProvider() string      { return h.providerID }
func (h *fakeHost) Signal() context.Context    { return h.signal }

func (h *fakeHost) Notify(message, level string) {
	h.notifies = append(h.notifies, notifyCall{message: message, level: level})
}

func (h *fakeHost) Compact(opts map[string]any) { h.compacts = append(h.compacts, opts) }

func (h *fakeHost) FindModel(provider, modelID string) (map[string]any, bool) {
	h.finds++
	return nil, false
}

func (h *fakeHost) Settings() (map[string]any, error) { return h.settings, h.settingsErr }
func (h *fakeHost) Usage() (usageView, error)         { return h.usage, h.usageErr }

func (h *fakeHost) Stream(model, request map[string]any, onPayload func(any, map[string]any) (any, error)) (map[string]any, error) {
	h.streams++
	return nil, nil
}

// countingDriver stands in for a Driver, so a test can assert whether the
// handler reached it at all without compiling a policy.
type countingDriver struct {
	beforeRequestCalls int
	beforeCompactCalls int
	payloads           []any
	last               CompactEvent
	result             map[string]any
}

var _ compactionDriver = (*countingDriver)(nil)

func (d *countingDriver) BeforeProviderRequest(sessionID, modelID, provider string, payload any) any {
	d.beforeRequestCalls++
	d.payloads = append(d.payloads, payload)
	return nil
}

func (d *countingDriver) BeforeCompact(ctx context.Context, sessionID, modelID, provider string, ev CompactEvent) map[string]any {
	d.beforeCompactCalls++
	d.last = ev
	return d.result
}

func (d *countingDriver) calls() int { return d.beforeRequestCalls + d.beforeCompactCalls }

// newTestEndpoint builds an endpoint without compiling the policy or opening a
// real trace file: the trace lands in a recording sink, and the driver seam is
// still the production one until a test replaces it.
// TestEvictionTraceNamesTheCause: an evicted session costs one cold compaction and
// re-arms its breaker, so the trace says which session and why.
func TestEvictionTraceNamesTheCause(t *testing.T) {
	ep := newTestEndpoint(t)
	ep.onEviction("s-evicted")

	recs := ep.sink().records
	if len(recs) != 1 {
		t.Fatalf("trace records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Outcome != OutcomeEvicted {
		t.Errorf("outcome = %q, want %q", rec.Outcome, OutcomeEvicted)
	}
	if rec.SessionID != "s-evicted" {
		t.Errorf("sessionId = %q, want s-evicted", rec.SessionID)
	}
	if !strings.Contains(rec.Detail, "capture table was full") || !strings.Contains(rec.Detail, "falls back") {
		t.Errorf("detail = %q, want the capacity and the consequence", rec.Detail)
	}
}

func newTestEndpoint(t *testing.T) *endpoint {
	t.Helper()
	ep := &endpoint{
		plugin:    stubPlugin{},
		state:     NewState(),
		trace:     &recordingSink{},
		traceFile: filepath.Join(t.TempDir(), "pi-cc-trace.jsonl"),
		notified:  make(map[string]bool),
	}
	ep.newDriver = ep.newEventDriver
	return ep
}

func (ep *endpoint) useDriver(d compactionDriver) {
	ep.newDriver = func(hostCall) compactionDriver { return d }
}

func (ep *endpoint) sink() *recordingSink {
	return ep.trace.(*recordingSink)
}

// record appends one trace record through the decorating sink, the way the
// driver does.
func record(ep *endpoint, h hostCall, rec TraceRecord) {
	_ = notifySink{trace: ep.trace, ep: ep, host: h}.Append(rec)
}

// compactDataWithoutCutPoint is compactData as PiG sends it when it has not
// settled on where the kept history starts.
func compactDataWithoutCutPoint() map[string]any {
	data := compactData()
	delete(data["preparation"].(map[string]any), "firstKeptEntryId")
	return data
}

// --- /cc-compact -------------------------------------------------------------

func TestCompactCommandNowCarriesInstructions(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}

	if err := ep.onCompactCommand(h, "now 只保留文件名"); err != nil {
		t.Fatalf("onCompactCommand: %v", err)
	}
	if len(h.compacts) != 1 {
		t.Fatalf("Compact calls = %d, want 1", len(h.compacts))
	}
	if got := h.compacts[0]["customInstructions"]; got != "只保留文件名" {
		t.Fatalf("customInstructions = %#v, want %q", got, "只保留文件名")
	}
	if len(h.notifies) != 0 {
		t.Fatalf("notifies = %#v, want none", h.notifies)
	}
}

func TestCompactCommandBareNowOmitsInstructions(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}

	if err := ep.onCompactCommand(h, "now"); err != nil {
		t.Fatalf("onCompactCommand: %v", err)
	}
	if len(h.compacts) != 1 {
		t.Fatalf("Compact calls = %d, want 1", len(h.compacts))
	}
	if _, present := h.compacts[0]["customInstructions"]; present {
		t.Fatalf("customInstructions present: %#v, want the key omitted", h.compacts[0])
	}
	if len(h.compacts[0]) != 0 {
		t.Fatalf("Compact options = %#v, want empty", h.compacts[0])
	}
}

func TestCompactCommandStatusDoesNotCompact(t *testing.T) {
	for _, args := range []string{"", "status"} {
		name := args
		if name == "" {
			name = "bare"
		}
		t.Run(name, func(t *testing.T) {
			ep := newTestEndpoint(t)
			h := &fakeHost{sessionID: "s1"}

			if err := ep.onCompactCommand(h, args); err != nil {
				t.Fatalf("onCompactCommand(%q): %v", args, err)
			}
			if len(h.compacts) != 0 {
				t.Fatalf("Compact calls = %d, want 0", len(h.compacts))
			}
			if len(h.notifies) != 1 {
				t.Fatalf("notifies = %#v, want exactly one", h.notifies)
			}
			if got := h.notifies[0].level; got != notifyInfo {
				t.Fatalf("notify level = %q, want %q", got, notifyInfo)
			}
		})
	}
}

func TestCompactCommandStatusShowsCaptureAndTracePath(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}
	ep.state.Put("s1", "group/deepseek", "magpie",
		json.RawMessage(`{"model":"group/deepseek","messages":[]}`))

	if err := ep.onCompactCommand(h, "status"); err != nil {
		t.Fatalf("onCompactCommand: %v", err)
	}
	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %d, want 1", len(h.notifies))
	}
	message := h.notifies[0].message
	for _, want := range []string{ep.traceFile, "group/deepseek", "magpie", "capture:", "breaker:", "plugin:"} {
		if !strings.Contains(message, want) {
			t.Fatalf("status = %q, want it to contain %q", message, want)
		}
	}
	// The payload size is part of the report.
	if !strings.Contains(message, "bytes") {
		t.Fatalf("status = %q, want the payload size", message)
	}
}

func TestCompactCommandStatusWithoutCapture(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}

	if err := ep.onCompactCommand(h, "status"); err != nil {
		t.Fatalf("onCompactCommand: %v", err)
	}
	if got := h.notifies[0].message; !strings.Contains(got, "capture: none") {
		t.Fatalf("status = %q, want %q", got, "capture: none")
	}
	if got := h.notifies[0].message; !strings.Contains(got, embeddedPlugin) {
		t.Fatalf("status = %q, want the plugin source %q", got, embeddedPlugin)
	}
}

func TestCompactCommandStatusShowsLoadError(t *testing.T) {
	ep := newTestEndpoint(t)
	ep.plugin = nil
	ep.loadErr = errors.New("pig-cc-compaction: read PIG_CC_PLUGIN: no such file")
	h := &fakeHost{sessionID: "s1"}

	if err := ep.onCompactCommand(h, ""); err != nil {
		t.Fatalf("onCompactCommand: %v", err)
	}
	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %d, want 1", len(h.notifies))
	}
	if got := h.notifies[0].message; !strings.Contains(got, ep.loadErr.Error()) {
		t.Fatalf("status = %q, want the load error %q", got, ep.loadErr.Error())
	}
}

func TestCompactCommandReset(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}
	ep.state.Put("s1", "group/deepseek", "magpie", json.RawMessage(`{"a":1}`))
	ep.state.NoteFailure("s1")
	if _, ok := ep.state.Get("s1"); !ok {
		t.Fatal("setup: no capture")
	}

	if err := ep.onCompactCommand(h, "reset"); err != nil {
		t.Fatalf("onCompactCommand: %v", err)
	}
	if _, ok := ep.state.Get("s1"); ok {
		t.Fatal("capture survived reset")
	}
	if got := ep.state.Breaker("s1"); got != (Breaker{}) {
		t.Fatalf("breaker after reset = %#v, want zero", got)
	}
	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %#v, want exactly one", h.notifies)
	}
	if !strings.Contains(h.notifies[0].message, "re-armed") {
		t.Fatalf("notify = %q, want it to say the breaker was re-armed", h.notifies[0].message)
	}
}

// TestCompactCommandResetRearmsNotifications checks the other half of reset: the
// messages a thrashing session already saw can be reported again.
func TestCompactCommandResetRearmsNotifications(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}
	failed := TraceRecord{SessionID: "s1", Outcome: OutcomeFailed, Detail: "network timeout"}

	record(ep, h, failed)
	record(ep, h, failed)
	if len(h.notifies) != 1 {
		t.Fatalf("notifies after two identical failures = %d, want 1", len(h.notifies))
	}

	if err := ep.onCompactCommand(h, "reset"); err != nil {
		t.Fatalf("onCompactCommand: %v", err)
	}
	record(ep, h, failed)
	if len(h.notifies) != 3 {
		t.Fatalf("notifies after reset = %d, want 3 (failure, reset, failure)", len(h.notifies))
	}
}

func TestCompactCommandUnknownArgument(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}

	if err := ep.onCompactCommand(h, "please compact"); err != nil {
		t.Fatalf("onCompactCommand: %v", err)
	}
	if len(h.compacts) != 0 {
		t.Fatalf("Compact calls = %d, want 0", len(h.compacts))
	}
	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %d, want 1", len(h.notifies))
	}
	if got := h.notifies[0].level; got != notifyWarning {
		t.Fatalf("notify level = %q, want %q", got, notifyWarning)
	}
	if !strings.Contains(h.notifies[0].message, "usage") {
		t.Fatalf("notify = %q, want the usage line", h.notifies[0].message)
	}
}

// --- notification decorator --------------------------------------------------

func TestPromptSinkReportsFailureOnce(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}

	record(ep, h, TraceRecord{SessionID: "s1", Outcome: OutcomeFailed, Detail: "provider 500"})
	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %d, want 1", len(h.notifies))
	}
	if got := h.notifies[0].level; got != notifyError {
		t.Fatalf("notify level = %q, want %q", got, notifyError)
	}
	if !strings.Contains(h.notifies[0].message, "provider 500") {
		t.Fatalf("notify = %q, want it to carry the detail", h.notifies[0].message)
	}
	if !strings.Contains(h.notifies[0].message, "Error during compaction") {
		t.Fatalf("notify = %q, want the §4.6 wording", h.notifies[0].message)
	}

	// The same outcome twice in a row is one message.
	record(ep, h, TraceRecord{SessionID: "s1", Outcome: OutcomeFailed, Detail: "provider 500"})
	if len(h.notifies) != 1 {
		t.Fatalf("notifies after a repeat = %d, want 1", len(h.notifies))
	}

	// A different reason is worth saying.
	record(ep, h, TraceRecord{SessionID: "s1", Outcome: OutcomeFailed, Detail: "timed out"})
	if len(h.notifies) != 2 {
		t.Fatalf("notifies after a new reason = %d, want 2", len(h.notifies))
	}
}

func TestPromptSinkReportsThrashing(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}
	for range thrashLimit {
		ep.state.NoteSuccess("s1")
	}
	if got := ep.state.Breaker("s1").Thrash; got < thrashLimit {
		t.Fatalf("setup: thrash = %d, want >= %d", got, thrashLimit)
	}

	record(ep, h, TraceRecord{
		SessionID: "s1",
		Outcome:   OutcomeFallback,
		Detail:    "circuit breaker open: automatic compaction failed or thrashed too often for this session",
	})
	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %#v, want exactly one", h.notifies)
	}
	if got := h.notifies[0].level; got != notifyWarning {
		t.Fatalf("notify level = %q, want %q", got, notifyWarning)
	}
	if !strings.Contains(h.notifies[0].message, "thrashing") {
		t.Fatalf("notify = %q, want it to mention thrashing", h.notifies[0].message)
	}
}

func TestPromptSinkReportsPausedBreaker(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}
	for range failLimit {
		ep.state.NoteFailure("s1")
	}

	record(ep, h, TraceRecord{
		SessionID: "s1",
		Outcome:   OutcomeFallback,
		Detail:    "circuit breaker open: automatic compaction failed or thrashed too often for this session",
	})
	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %#v, want exactly one", h.notifies)
	}
	if got := h.notifies[0].level; got != notifyWarning {
		t.Fatalf("notify level = %q, want %q", got, notifyWarning)
	}
	if !strings.Contains(h.notifies[0].message, "paused") {
		t.Fatalf("notify = %q, want it to mention the pause", h.notifies[0].message)
	}
}

// TestPromptSinkReportsUnusableSummary covers §4.6 row 2: the stricter re-ask
// also came back without a usable <summary>, so the user is told why PiG had to
// compact by itself — with the size evidence the trace record carries.
func TestPromptSinkReportsUnusableSummary(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}
	rec := TraceRecord{
		SessionID:       "s1",
		Outcome:         OutcomeFallback,
		Detail:          unusableSummaryDetail,
		InputTokens:     3000,
		CacheReadTokens: 12000,
		OutputTokens:    512,
	}

	record(ep, h, rec)
	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %#v, want exactly one", h.notifies)
	}
	if got := h.notifies[0].level; got != notifyWarning {
		t.Fatalf("notify level = %q, want %q", got, notifyWarning)
	}
	message := h.notifies[0].message
	for _, want := range []string{summaryUnusablePrefix, unusableSummaryDetail, "512", "3000", "12000"} {
		if !strings.Contains(message, want) {
			t.Fatalf("notify = %q, want it to contain %q", message, want)
		}
	}
}

// TestPromptSinkUnusableSummaryIgnoresCase keeps the branch from depending on
// the driver's capitalisation.
func TestPromptSinkUnusableSummaryIgnoresCase(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}

	record(ep, h, TraceRecord{
		SessionID: "s1",
		Outcome:   OutcomeFallback,
		Detail:    "No Summary After Two Attempts",
	})
	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %#v, want exactly one", h.notifies)
	}
	if got := h.notifies[0].level; got != notifyWarning {
		t.Fatalf("notify level = %q, want %q", got, notifyWarning)
	}
}

// TestPromptSinkUnusableSummaryWithoutUsage keeps the message honest when the
// record carries no token counts: no empty parenthetical, no invented numbers.
func TestPromptSinkUnusableSummaryWithoutUsage(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}

	record(ep, h, TraceRecord{SessionID: "s1", Outcome: OutcomeFallback, Detail: unusableSummaryDetail})
	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %#v, want exactly one", h.notifies)
	}
	if got := h.notifies[0].message; got != summaryUnusablePrefix+unusableSummaryDetail {
		t.Fatalf("notify = %q, want exactly %q", got, summaryUnusablePrefix+unusableSummaryDetail)
	}
	if strings.Contains(h.notifies[0].message, "tokens") {
		t.Fatalf("notify = %q, want no size evidence without usage", h.notifies[0].message)
	}
}

// TestPromptSinkUnusableSummaryDedupedUntilReset keeps a session that keeps
// returning an unparsable summary from being told on every turn.
func TestPromptSinkUnusableSummaryDedupedUntilReset(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}
	rec := TraceRecord{
		SessionID:    "s1",
		Outcome:      OutcomeFallback,
		Detail:       unusableSummaryDetail,
		OutputTokens: 512,
	}

	record(ep, h, rec)
	record(ep, h, rec)
	if len(h.notifies) != 1 {
		t.Fatalf("notifies after two identical records = %d, want 1", len(h.notifies))
	}

	if err := ep.onCompactCommand(h, compactResetArg); err != nil {
		t.Fatalf("onCompactCommand: %v", err)
	}
	record(ep, h, rec)
	if len(h.notifies) != 3 {
		t.Fatalf("notifies after reset = %d, want 3 (summary, reset, summary)", len(h.notifies))
	}
	if got := h.notifies[2].level; got != notifyWarning {
		t.Fatalf("notify level after reset = %q, want %q", got, notifyWarning)
	}
	if !strings.Contains(h.notifies[2].message, unusableSummaryDetail) {
		t.Fatalf("notify after reset = %q, want the summary reason", h.notifies[2].message)
	}
}

// TestPromptSinkBreakerBeatsSummary is the regression guard for the branch
// order: a reason that names the breaker never reports as a summary problem,
// even when it also mentions the exhausted retries.
func TestPromptSinkBreakerBeatsSummary(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}
	detail := breakerOpenDetail + ": automatic compaction failed or thrashed too often; " + unusableSummaryDetail

	record(ep, h, TraceRecord{SessionID: "s1", Outcome: OutcomeFallback, Detail: detail})
	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %#v, want exactly one", h.notifies)
	}
	message := h.notifies[0].message
	if strings.Contains(message, summaryUnusablePrefix) {
		t.Fatalf("notify = %q, want the breaker message, not the summary one", message)
	}
	if !strings.Contains(message, "paused") {
		t.Fatalf("notify = %q, want the paused-breaker message", message)
	}
}

// TestPromptSinkSilentPassThrough pins what must NOT reach the user: overflow, a
// session with no capture yet, an unusable payload and every successful takeover
// are the extension working as designed. (The exhausted-retry summary is not on
// this list: §4.6 row 2 asks for a prompt, see the tests above.)
func TestPromptSinkSilentPassThrough(t *testing.T) {
	details := []string{
		"",
		"overflow: the resend would exceed the window",
		"no captured request: nothing was captured for this session",
		"no request has been answered yet in this session",
		"unusable payload",
	}
	for _, detail := range details {
		t.Run(detail, func(t *testing.T) {
			ep := newTestEndpoint(t)
			h := &fakeHost{sessionID: "s1"}
			record(ep, h, TraceRecord{SessionID: "s1", Outcome: OutcomeFallback, Detail: detail})
			if len(h.notifies) != 0 {
				t.Fatalf("notify for fallback %q = %#v, want none", detail, h.notifies)
			}
		})
	}

	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}
	record(ep, h, TraceRecord{SessionID: "s1", Outcome: OutcomeCompacted})
	if len(h.notifies) != 0 {
		t.Fatalf("notify for a successful takeover = %#v, want none", h.notifies)
	}
}

func TestPromptSinkForwardsEveryRecord(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}
	want := TraceRecord{
		SessionID:       "s1",
		Outcome:         OutcomeFailed,
		Detail:          "provider 500",
		InputTokens:     10,
		CacheReadTokens: 900,
		OutputTokens:    20,
		DurationMs:      42,
	}

	record(ep, h, want)

	sink := ep.sink()
	if len(sink.records) != 1 {
		t.Fatalf("trace records = %d, want 1", len(sink.records))
	}
	if !reflect.DeepEqual(sink.records[0], want) {
		t.Fatalf("trace record = %#v, want %#v", sink.records[0], want)
	}
}

// --- session_before_compact --------------------------------------------------

func TestSessionBeforeCompactWithoutCutPoint(t *testing.T) {
	ep := newTestEndpoint(t)
	drv := &countingDriver{}
	ep.useDriver(drv)
	h := &fakeHost{sessionID: "s1", modelID: "group/deepseek", providerID: "magpie"}

	got, err := ep.onSessionBeforeCompact(h, compactDataWithoutCutPoint())
	if err != nil {
		t.Fatalf("onSessionBeforeCompact: %v", err)
	}
	if got != nil {
		t.Fatalf("result = %#v, want nil", got)
	}
	if drv.calls() != 0 {
		t.Fatalf("driver calls = %d, want 0", drv.calls())
	}

	sink := ep.sink()
	if len(sink.records) != 1 {
		t.Fatalf("trace records = %d, want 1", len(sink.records))
	}
	if got := sink.records[0].Outcome; got != OutcomeFallback {
		t.Fatalf("trace outcome = %q, want %q", got, OutcomeFallback)
	}
	if got := sink.records[0].Detail; got != noCutPointDetail {
		t.Fatalf("trace detail = %q, want %q", got, noCutPointDetail)
	}
	if got := sink.records[0].SessionID; got != "s1" {
		t.Fatalf("trace session = %q, want s1", got)
	}

	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %#v, want exactly one", h.notifies)
	}
	if !strings.Contains(h.notifies[0].message, noCutPointDetail) {
		t.Fatalf("notify = %q, want it to carry %q", h.notifies[0].message, noCutPointDetail)
	}
}

func TestSessionBeforeCompactPassesThrough(t *testing.T) {
	ep := newTestEndpoint(t)
	want := map[string]any{"compaction": map[string]any{"summary": "s"}}
	drv := &countingDriver{result: want}
	ep.useDriver(drv)
	h := &fakeHost{sessionID: "s1", modelID: "group/deepseek", providerID: "magpie",
		signal: context.Background()}

	got, err := ep.onSessionBeforeCompact(h, compactData())
	if err != nil {
		t.Fatalf("onSessionBeforeCompact: %v", err)
	}
	if !reflect.DeepEqual(got, any(want)) {
		t.Fatalf("result = %#v, want %#v", got, want)
	}
	if drv.beforeCompactCalls != 1 {
		t.Fatalf("BeforeCompact calls = %d, want 1", drv.beforeCompactCalls)
	}
	if drv.last.Preparation.FirstKeptEntryID != "e9" {
		t.Fatalf("FirstKeptEntryID = %q, want e9", drv.last.Preparation.FirstKeptEntryID)
	}
}

// TestSessionBeforeCompactWithoutPayloadOrSession covers the two silent bail-outs
// before the cut-point check.
func TestSessionBeforeCompactWithoutPayloadOrSession(t *testing.T) {
	t.Run("no preparation", func(t *testing.T) {
		ep := newTestEndpoint(t)
		drv := &countingDriver{}
		ep.useDriver(drv)
		h := &fakeHost{sessionID: "s1"}

		got, err := ep.onSessionBeforeCompact(h, map[string]any{"reason": "manual"})
		if err != nil || got != nil {
			t.Fatalf("onSessionBeforeCompact = %#v, %v; want nil, nil", got, err)
		}
		if drv.calls() != 0 || len(h.notifies) != 0 || len(ep.sink().records) != 0 {
			t.Fatalf("bail-out touched the host: driver=%d notifies=%d records=%d",
				drv.calls(), len(h.notifies), len(ep.sink().records))
		}
	})

	t.Run("no session id", func(t *testing.T) {
		ep := newTestEndpoint(t)
		drv := &countingDriver{}
		ep.useDriver(drv)
		h := &fakeHost{sessionErr: errors.New("no session")}

		got, err := ep.onSessionBeforeCompact(h, compactData())
		if err != nil || got != nil {
			t.Fatalf("onSessionBeforeCompact = %#v, %v; want nil, nil", got, err)
		}
		if drv.calls() != 0 || len(h.notifies) != 0 {
			t.Fatalf("bail-out touched the host: driver=%d notifies=%d", drv.calls(), len(h.notifies))
		}
	})
}

// --- before_provider_request / session_shutdown ------------------------------

func TestBeforeProviderRequestCapturesPayload(t *testing.T) {
	ep := newTestEndpoint(t)
	drv := &countingDriver{}
	ep.useDriver(drv)
	h := &fakeHost{sessionID: "s1", modelID: "group/deepseek", providerID: "magpie"}
	payload := map[string]any{"model": "group/deepseek", "messages": []any{}}

	got, err := ep.onBeforeProviderRequest(h, map[string]any{"payload": payload})
	if err != nil {
		t.Fatalf("onBeforeProviderRequest: %v", err)
	}
	if got != nil {
		t.Fatalf("result = %#v, want nil (the host keeps its payload)", got)
	}
	if drv.beforeRequestCalls != 1 {
		t.Fatalf("BeforeProviderRequest calls = %d, want 1", drv.beforeRequestCalls)
	}
	if len(drv.payloads) != 1 || !reflect.DeepEqual(drv.payloads[0], any(payload)) {
		t.Fatalf("payload = %#v, want %#v", drv.payloads, payload)
	}
	if len(h.notifies) != 0 {
		t.Fatalf("notifies = %#v, want none", h.notifies)
	}
}

func TestBeforeProviderRequestIgnoresEnvelopeWithoutPayload(t *testing.T) {
	ep := newTestEndpoint(t)
	drv := &countingDriver{}
	ep.useDriver(drv)
	h := &fakeHost{sessionID: "s1"}

	if got, err := ep.onBeforeProviderRequest(h, map[string]any{"type": "before_provider_request"}); got != nil || err != nil {
		t.Fatalf("onBeforeProviderRequest = %#v, %v; want nil, nil", got, err)
	}
	if drv.calls() != 0 {
		t.Fatalf("driver calls = %d, want 0", drv.calls())
	}
}

func TestSessionShutdownForgetsSession(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}
	ep.state.Put("s1", "group/deepseek", "magpie", json.RawMessage(`{"a":1}`))

	if got, err := ep.onSessionShutdown(h, nil); got != nil || err != nil {
		t.Fatalf("onSessionShutdown = %#v, %v; want nil, nil", got, err)
	}
	if _, ok := ep.state.Get("s1"); ok {
		t.Fatal("capture survived session_shutdown")
	}
}

// --- tracePath ---------------------------------------------------------------

func TestTracePathWithoutHome(t *testing.T) {
	t.Setenv(traceEnv, "")
	t.Setenv("HOME", "")

	got := tracePath()
	if !strings.HasSuffix(got, filepath.Join(".pi", "agent", "pi-cc-trace.jsonl")) {
		t.Fatalf("tracePath() = %q, want the default suffix", got)
	}
}

// --- session_start -----------------------------------------------------------

func TestSessionStartReportsLoadFailureOnce(t *testing.T) {
	ep := newTestEndpoint(t)
	ep.plugin = nil
	ep.loadErr = errors.New("pig-cc-compaction: read PIG_CC_PLUGIN: no such file")
	h := &fakeHost{sessionID: "s1"}

	for range 2 {
		if got, err := ep.onSessionStart(h, nil); got != nil || err != nil {
			t.Fatalf("onSessionStart = %#v, %v; want nil, nil", got, err)
		}
	}
	if len(h.notifies) != 1 {
		t.Fatalf("notifies = %#v, want exactly one for two session starts", h.notifies)
	}
	if got := h.notifies[0].level; got != notifyError {
		t.Fatalf("notify level = %q, want %q", got, notifyError)
	}
	if !strings.Contains(h.notifies[0].message, ep.loadErr.Error()) {
		t.Fatalf("notify = %q, want it to carry the load error %q",
			h.notifies[0].message, ep.loadErr.Error())
	}
}

func TestSessionStartQuietWhenPluginLoaded(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1"}

	if got, err := ep.onSessionStart(h, nil); got != nil || err != nil {
		t.Fatalf("onSessionStart = %#v, %v; want nil, nil", got, err)
	}
	if len(h.notifies) != 0 {
		t.Fatalf("notifies = %#v, want none", h.notifies)
	}
}

// TestSettingsFromRawFallsBackToPisDefaults: a settings file without a
// compaction section still reports the thresholds the host will use.
func TestSettingsFromRawFallsBackToPisDefaults(t *testing.T) {
	for _, raw := range []map[string]any{nil, {}, {"compaction": "not an object"}} {
		view := settingsFromRaw(raw)
		if !view.Enabled || view.ReserveTokens != defaultReserveTokens || view.KeepRecentTokens != defaultKeepRecentTokens {
			t.Fatalf("settingsFromRaw(%#v) = %#v, want Pi's defaults", raw, view)
		}
	}

	view := settingsFromRaw(map[string]any{"compaction": map[string]any{
		"enabled":          false,
		"reserveTokens":    float64(1000),
		"keepRecentTokens": float64(8000),
	}})
	if view.Enabled || view.ReserveTokens != 1000 || view.KeepRecentTokens != 8000 {
		t.Fatalf("settingsFromRaw() = %#v, want the file's values", view)
	}
}

// TestSettingsReport: the status line names the two settings that stop
// compaction before the extension is ever asked, and stays quiet otherwise.
func TestSettingsReport(t *testing.T) {
	cases := []struct {
		name    string
		view    settingsView
		usage   usageView
		want    []string
		notWant []string
	}{
		{
			name:  "healthy",
			view:  settingsView{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 8000},
			usage: usageView{Tokens: 120000, ContextWindow: 128000, Known: true},
		},
		{
			name:  "disabled",
			view:  settingsView{Enabled: false, ReserveTokens: 16384, KeepRecentTokens: 8000},
			usage: usageView{Tokens: 120000, ContextWindow: 128000, Known: true},
			want:  []string{"compaction.enabled is false"},
		},
		{
			name:  "session too small to compact",
			view:  settingsView{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 20000},
			usage: usageView{Tokens: 5000, ContextWindow: 128000, Known: true},
			want:  []string{"Nothing to compact (session too small)", "keepRecent=20000"},
		},
		{
			name:  "keeps more than half the window",
			view:  settingsView{Enabled: true, ReserveTokens: 16384, KeepRecentTokens: 100000},
			usage: usageView{Tokens: 120000, ContextWindow: 128000, Known: true},
			want:  []string{"more than half the 128000-token window"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := settingsReport(tc.view, tc.usage)
			joined := strings.Join(lines, "\n")
			if !strings.HasPrefix(lines[0], "compaction: enabled=") {
				t.Fatalf("first line = %q, want the thresholds", lines[0])
			}
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Fatalf("report = %q, want it to contain %q", joined, want)
				}
			}
			if len(tc.want) == 0 && strings.Contains(joined, "warning:") {
				t.Fatalf("report = %q, want no warning", joined)
			}
		})
	}
}

// TestStatusReportsTheSessionTooSmallTrap: the case that leaves no trace at all,
// because the host never emits the compaction event, is visible in status.
func TestStatusReportsTheSessionTooSmallTrap(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{
		sessionID: "s1",
		settings: map[string]any{"compaction": map[string]any{
			"keepRecentTokens": float64(20000),
			"reserveTokens":    float64(20000),
		}},
		usage: usageView{Tokens: 5000, ContextWindow: 128000, Known: true},
	}

	if err := ep.onCompactCommand(h, "status"); err != nil {
		t.Fatalf("onCompactCommand: %v", err)
	}
	message := h.notifies[0].message
	for _, want := range []string{
		"compaction: enabled=true reserve=20000 keepRecent=20000 window=128000",
		"Nothing to compact (session too small)",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("status = %q, want it to contain %q", message, want)
		}
	}
}

// TestStatusSurvivesAHostWithoutSettings: the status command is a diagnostic, so
// a host that cannot answer still gets the rest of the report.
func TestStatusSurvivesAHostWithoutSettings(t *testing.T) {
	ep := newTestEndpoint(t)
	h := &fakeHost{sessionID: "s1", settingsErr: errors.New("the host sent no settings")}

	if err := ep.onCompactCommand(h, "status"); err != nil {
		t.Fatalf("onCompactCommand: %v", err)
	}
	if got := h.notifies[0].message; !strings.Contains(got, "compaction: unavailable (the host sent no settings)") {
		t.Fatalf("status = %q, want the settings failure named", got)
	}
}

// TestDecodeCompactEventKeepsTheTurnPrefix: the checkpoint request is built from
// these messages, so a decoder that drops them turns the split-turn path into a
// silent no-op. It did, until this test existed.
func TestDecodeCompactEventKeepsTheTurnPrefix(t *testing.T) {
	data := compactData()
	prep := data["preparation"].(map[string]any)
	prep["isSplitTurn"] = true
	prep["turnPrefixMessages"] = []any{
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "fix the flaky test"}}},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "looking at the runner"}}},
	}

	ev, ok := decodeCompactEvent(data)
	if !ok {
		t.Fatal("decodeCompactEvent rejected a valid event")
	}
	if !ev.Preparation.IsSplitTurn {
		t.Fatal("IsSplitTurn = false, want true")
	}
	if len(ev.Preparation.TurnPrefixMessages) != 2 {
		t.Fatalf("TurnPrefixMessages = %d, want 2", len(ev.Preparation.TurnPrefixMessages))
	}
	if !strings.Contains(string(ev.Preparation.TurnPrefixMessages[1]), "looking at the runner") {
		t.Fatalf("second message = %s, want the assistant turn", ev.Preparation.TurnPrefixMessages[1])
	}
}
