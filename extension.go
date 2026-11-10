package pigcc

// This is the only file that imports the PiG host SDK: everything here turns
// the host's events, command and model calls into calls on the host-free core,
// so every other file stays plain Go a unit test can run without a host.
//
// The sdk sits behind hostCall, a narrow interface this file adapts an
// sdk.Context to for the length of one handler call. That seam is what keeps
// the handlers testable: sdk.Context only exists inside a running PiG, and the
// endpoint never stores one.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

const (
	// extensionName is the factory's identity: it has to match the name of the
	// selected Package, Piglet, or extension path.
	extensionName = "pig-cc-compaction"

	// pluginEnv names a JavaScript file to compile instead of the embedded
	// plugin/compaction.js, so the prompt can be reworded without a rebuild.
	pluginEnv = "PIG_CC_PLUGIN"

	// traceEnv overrides where the trace log is appended.
	traceEnv = "PIG_CC_TRACE"

	// compactCommand is the manual trigger, PiG's /cc-compact.
	compactCommand = "cc-compact"

	// compactDescription is the line PiG shows in its command list.
	compactDescription = "Compact now, using a Claude Code style summary request."

	// The three arguments /cc-compact understands, and the reminder for the
	// rest.
	compactStatusArg = "status"
	compactResetArg  = "reset"
	compactNowArg    = "now"
	compactUsage     = "usage: /cc-compact [status|reset|now [instructions]]"

	// embeddedPlugin is what the status report calls the built-in policy.
	embeddedPlugin = "embedded"
)

const (
	// notifyInfo, notifyWarning and notifyError are Pi's notification levels.
	notifyInfo    = "info"
	notifyWarning = "warning"
	notifyError   = "error"
)

const (
	// breakerOpenDetail is the substring the policy puts in a fallback reason
	// when the breaker is what kept the extension from taking over. Every other
	// fallback — overflow, no capture yet, an unusable payload, a plan that
	// declines — is the extension working as designed and stays silent.
	breakerOpenDetail = "circuit breaker open"

	// breakerThrashingMessage and breakerPausedMessage say why automatic
	// takeover stopped and how to get it back (§4.6: the user has to see this,
	// not only the trace log).
	breakerThrashingMessage = "Autocompact is thrashing, so this session is falling back to PiG's compaction; /cc-compact reset re-arms it"
	breakerPausedMessage    = "Autocompact is paused for this session after repeated failures; /cc-compact now still works"

	// unusableSummaryDetail is the driver's exact text for the exhausted-retry
	// path (§4.6 row 2: the stricter re-ask also came back without a usable
	// <summary>). Matching it is what tells the user why PiG had to compact by
	// itself; the record itself is a fallback like any other.
	unusableSummaryDetail = "no summary after two attempts"

	// summaryUnusablePrefix opens the §4.6 row-2 notification. The rest of the
	// message is the reason the driver recorded plus the size evidence the trace
	// record actually carries: stopReason and a snippet exist only on the JS
	// side, and inventing them here would be worse than omitting them.
	summaryUnusablePrefix = "compaction summary unusable: "

	// noCutPointDetail is the trace reason for the one inconsistency the
	// extension refuses to hand PiG: a compaction result with no cut point.
	noCutPointDetail = "no firstKeptEntryId in the compaction preparation"
)

// errNoStream reports a host that answered a stream request with no stream. It
// is a bail-out like any other: the driver records it and PiG compacts by
// itself.
var errNoStream = errors.New("pigcc: host returned no model stream")

// Extension is the factory PiG loads.
//
// It registers the two hooks that make the takeover possible — capture the
// request body the session last sent, then answer session_before_compact from
// it — plus session_shutdown, which drops the captured body, and the
// /cc-compact command that inspects, resets and triggers the takeover.
//
// The policy, the session table and the trace sink live as long as the
// extension process. Anything that needs the host — the model registry, the
// model stream, notifications — is taken from the handler's sdk.Context for
// the length of that handler and thrown away afterwards, so concurrent handlers
// for different sessions never share a Context.
//
// A plugin source that does not compile is not fatal: the extension still
// loads, tells the session why it cannot compact, and leaves every compaction
// to PiG.
func Extension() *sdk.Extension {
	e, _ := newExtension()
	return e
}

// newExtension builds the extension and hands back the endpoint behind it.
//
// The endpoint is what a test can inspect: sdk keeps its declarations private,
// so the endpoint keeps the ledger of the declarations it made. Handler code
// only ever needs the *sdk.Extension, which is what Extension returns.
func newExtension() (*sdk.Extension, *endpoint) {
	ep := newEndpoint()
	e := sdk.New(extensionName)

	if ep.plugin == nil {
		// The policy did not load. Register the notice — OnEvent(session_start)
		// is what sdk.OnSessionStart wraps — and the manual trigger, and leave
		// the compaction path unregistered: there is nothing behind it but an
		// error. /cc-compact status still works and names the error.
		ep.addEvent(e, sdk.EventSessionStart, eventFunc(ep.onSessionStart))
		ep.addCommand(e, compactCommand, compactDescription, commandFunc(ep.onCompactCommand))
		return e, ep
	}

	ep.addEvent(e, sdk.EventBeforeProviderRequest, eventFunc(ep.onBeforeProviderRequest))
	ep.addEvent(e, sdk.EventSessionBeforeCompact, eventFunc(ep.onSessionBeforeCompact))
	ep.addEvent(e, sdk.EventSessionShutdown, eventFunc(ep.onSessionShutdown))
	ep.addCommand(e, compactCommand, compactDescription, commandFunc(ep.onCompactCommand))
	return e, ep
}

// endpoint is the part of the extension that outlives a single event: the
// compiled policy, the per-session captures and the trace log. Every field is
// either immutable after construction or guarded by mu, because PiG may run
// handlers for different sessions at the same time.
type endpoint struct {
	plugin    Plugin
	loadErr   error
	state     State
	trace     Sink
	traceFile string

	// newDriver builds the host bridge for one event. It is a field so a test
	// can count driver calls without a host; production always uses
	// newEventDriver.
	newDriver newDriverFunc

	mu       sync.Mutex
	notified map[string]bool
	declared []string
}

// newEndpoint compiles the policy and opens the process-wide state. A plugin
// that fails to compile is kept as loadErr, never as a panic.
func newEndpoint() *endpoint {
	state := NewState()
	ep := &endpoint{
		state:    state,
		notified: make(map[string]bool),
	}
	ep.traceFile = tracePath()
	ep.trace = NewTrace(ep.traceFile)
	ep.newDriver = ep.newEventDriver
	state.OnEvict = ep.onEviction
	ep.plugin, ep.loadErr = loadPlugin()
	return ep
}

// onEviction records a session the capture table dropped. Losing it costs that
// session one cold compaction and re-arms its breaker, and nothing else reports
// it, so the trace gets a line that names the cause.
func (ep *endpoint) onEviction(sessionID string) {
	ep.appendTrace(TraceRecord{
		Outcome:   OutcomeEvicted,
		SessionID: sessionID,
		Detail: fmt.Sprintf("the capture table was full (%d sessions), so the least "+
			"recently used session was dropped; its next compaction falls back", maxSessions),
	})
}

// loadPlugin compiles the policy from PIG_CC_PLUGIN when it is set, and from the
// embedded plugin/compaction.js otherwise. A failure is returned, never
// panicked: a broken prompt file must not take the whole extension down.
func loadPlugin() (Plugin, error) {
	name, src := ModuleName, DefaultSource()
	if path := os.Getenv(pluginEnv); path != "" {
		body, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("pig-cc-compaction: read %s: %w", pluginEnv, err)
		}
		// The file is the module name: moejs reports it in stack traces, so a
		// policy loaded from disk points at the file that failed.
		name, src = path, string(body)
	}

	plugin, err := NewPlugin(name, src)
	if err != nil {
		return nil, err
	}
	if plugin == nil {
		return nil, errors.New("pig-cc-compaction: plugin source produced no plugin")
	}
	return plugin, nil
}

// pluginSource names where the policy came from, for /cc-compact status.
func pluginSource() string {
	if path := os.Getenv(pluginEnv); path != "" {
		return path
	}
	return embeddedPlugin
}

// addEvent subscribes fn to name and records the declaration. The record sits
// on the same call as the subscription, so the ledger cannot drift from what
// the host was told.
func (ep *endpoint) addEvent(e *sdk.Extension, name string, fn sdk.EventFunc) {
	e.OnEvent(name, fn)
	ep.record("event:" + name)
}

// addCommand declares a command and records the declaration.
func (ep *endpoint) addCommand(e *sdk.Extension, name, description string, fn sdk.CommandFunc) {
	e.Command(name, description, fn)
	ep.record("command:" + name)
}

func (ep *endpoint) record(declaration string) {
	ep.mu.Lock()
	ep.declared = append(ep.declared, declaration)
	ep.mu.Unlock()
}

// declarations copies the ledger, in registration order.
func (ep *endpoint) declarations() []string {
	ep.mu.Lock()
	defer ep.mu.Unlock()
	return append([]string(nil), ep.declared...)
}

// --- host seam ---------------------------------------------------------------

// hostCall is the slice of one sdk.Context the wiring uses. It exists so the
// handlers can run in a unit test: sdk.Context is handed out by a running host
// only, while this interface is what those calls mean.
type hostCall interface {
	SessionID() (string, error)
	Model() string
	ModelProvider() string
	Notify(message, level string)
	Compact(opts map[string]any)
	Signal() context.Context
	FindModel(provider, modelID string) (map[string]any, bool)
	Stream(model, request map[string]any, onPayload func(any, map[string]any) (any, error)) (map[string]any, error)
	// Settings is the effective global+project settings the host merged.
	Settings() (map[string]any, error)
	// Usage is the context-window usage of the session, known only after a
	// provider answered.
	Usage() (usageView, error)
}

// usageView is the part of the host's context usage the status line reports.
type usageView struct {
	Tokens        int
	ContextWindow int
	Known         bool
}

// sdkHost adapts one handler's sdk.Context. It is created per call and never
// stored, so no two handlers share a Context.
type sdkHost struct {
	ctx sdk.Context
}

var _ hostCall = sdkHost{}

func (h sdkHost) SessionID() (string, error) { return h.ctx.GetSessionID() }
func (h sdkHost) Model() string              { return h.ctx.Model() }
func (h sdkHost) ModelProvider() string      { return h.ctx.ModelProvider() }
func (h sdkHost) Notify(message, level string) {
	h.ctx.Notify(message, level)
}

func (h sdkHost) Compact(opts map[string]any) { h.ctx.Compact(opts) }

func (h sdkHost) Settings() (map[string]any, error) { return h.ctx.GetSettings() }

// Usage reports the context tokens and window. A host that does not know the
// usage sends no token count, which is the one case a warning cannot use.
func (h sdkHost) Usage() (usageView, error) {
	usage, err := h.ctx.GetContextUsage()
	if err != nil || usage == nil {
		return usageView{}, err
	}
	view := usageView{ContextWindow: usage.ContextWindow, Known: usage.Tokens != nil}
	if usage.Tokens != nil {
		view.Tokens = *usage.Tokens
	}
	return view, nil
}

func (h sdkHost) Signal() context.Context { return h.ctx.Signal() }

// FindModel resolves the provider/model pair through the host registry. A
// missing model is not an error the driver reports: it is a bail-out.
func (h sdkHost) FindModel(provider, modelID string) (map[string]any, bool) {
	model, err := h.ctx.ModelRegistry().Find(provider, modelID)
	if err != nil || model == nil {
		return nil, false
	}
	return model, true
}

// Stream sends one request through the host's streaming model call with the
// driver's onPayload installed, and blocks until the terminal message arrives:
// Result is the decoded assistant message, which is what the driver parses.
func (h sdkHost) Stream(model, request map[string]any, onPayload func(any, map[string]any) (any, error)) (map[string]any, error) {
	options := map[string]any{"onPayload": sdk.OnPayloadFunc(onPayload)}
	stream := h.ctx.ModelRegistry().Stream(model, request, options)
	if stream == nil {
		return nil, errNoStream
	}
	return stream.Result(), nil
}

// eventFunc adapts an endpoint handler to the sdk: the sdk's Context becomes the
// hostCall the handler sees, for the length of this one event.
func eventFunc(fn func(h hostCall, data map[string]any) (any, error)) sdk.EventFunc {
	return func(ctx sdk.Context, data map[string]any) (any, error) {
		return fn(sdkHost{ctx: ctx}, data)
	}
}

// commandFunc adapts an endpoint command handler to the sdk.
func commandFunc(fn func(h hostCall, args string) error) sdk.CommandFunc {
	return func(ctx sdk.Context, args string) error {
		return fn(sdkHost{ctx: ctx}, args)
	}
}

// sessionIDOf reads the session ID, mapping the host's error and an empty ID to
// the empty string: every caller treats "no session" the same way.
func sessionIDOf(h hostCall) string {
	sessionID, err := h.SessionID()
	if err != nil || sessionID == "" {
		return ""
	}
	return sessionID
}

// runContext is the cancellation the compaction work runs under: the run's
// signal, or a background context when no run is active, because the driver
// needs a usable context and the host only promises one during a run.
func runContext(h hostCall) context.Context {
	if signal := h.Signal(); signal != nil {
		return signal
	}
	return context.Background()
}

// --- driver bridge -----------------------------------------------------------

// compactionDriver is the part of a Driver an endpoint call reaches: the narrow
// slice a test can replace to count calls without a policy or a host.
type compactionDriver interface {
	BeforeProviderRequest(sessionID, modelID, provider string, payload any) any
	BeforeCompact(ctx context.Context, sessionID, modelID, provider string, ev CompactEvent) map[string]any
}

// newDriverFunc builds the host bridge for one event.
type newDriverFunc func(h hostCall) compactionDriver

// newEventDriver builds the host bridge for one event. The stream and the model
// lookups come from this event's host, and the trace sink is wrapped so the
// outcomes the design wants the user to see also reach the UI. The driver is
// discarded when the handler returns: it is cheap, and nothing about it is
// shared between handlers except the state and the sink.
func (ep *endpoint) newEventDriver(h hostCall) compactionDriver {
	return NewDriver(DriverOptions{
		Plugin:    ep.plugin,
		State:     ep.state,
		Trace:     notifySink{trace: ep.trace, ep: ep, host: h},
		FindModel: h.FindModel,
		Stream:    h.Stream,
	})
}

// notifySink decorates the trace sink. Every record is forwarded to the real
// sink unchanged, and the outcomes §4.6 requires the user to see become
// notifications.
type notifySink struct {
	trace Sink
	ep    *endpoint
	host  hostCall
}

func (s notifySink) Append(rec TraceRecord) error {
	var err error
	if s.trace != nil {
		err = s.trace.Append(rec)
	}
	s.ep.prompt(s.host, rec)
	return err
}

// prompt turns one trace record into the notification §4.6 asks for: a failure
// is reported with its reason, a summary the retries could not salvage says so
// with the evidence the record carries, and a breaker fallback says whether it
// tripped on repeated failures or on thrashing. Every other outcome is silent on
// purpose: overflow, a session with no capture yet, an unusable payload and a
// plan that declines are the extension working as designed.
func (ep *endpoint) prompt(h hostCall, rec TraceRecord) {
	switch rec.Outcome {
	case OutcomeFailed:
		ep.notifyOnce(h, rec.SessionID, "Error during compaction: "+rec.Detail, notifyError)
	case OutcomeFallback:
		switch {
		case strings.Contains(rec.Detail, breakerOpenDetail):
			// Checked first: a detail that names the breaker always reports the
			// breaker, never the summary.
			message := breakerPausedMessage
			if ep.state.Breaker(rec.SessionID).Thrash >= thrashLimit {
				message = breakerThrashingMessage
			}
			ep.notifyOnce(h, rec.SessionID, message, notifyWarning)
		case isUnusableSummary(rec.Detail):
			ep.notifyOnce(h, rec.SessionID, summaryUnusableMessage(rec), notifyWarning)
		}
	}
}

// isUnusableSummary reports whether a fallback reason is the driver's
// exhausted-retry reason. The comparison ignores case because the reason is
// text a future driver may capitalise.
func isUnusableSummary(detail string) bool {
	return strings.Contains(strings.ToLower(detail), unusableSummaryDetail)
}

// summaryUnusableMessage spells out the §4.6 row-2 prompt: what went wrong and
// how much was actually spent on the attempt. Only evidence the trace record
// carries is included, so a record without usage still produces a useful line.
func summaryUnusableMessage(rec TraceRecord) string {
	message := summaryUnusablePrefix + rec.Detail

	var parts []string
	if rec.OutputTokens != 0 {
		parts = append(parts, fmt.Sprintf("output %d tokens", rec.OutputTokens))
	}
	if rec.InputTokens != 0 || rec.CacheReadTokens != 0 {
		parts = append(parts, fmt.Sprintf("input %d cached %d", rec.InputTokens, rec.CacheReadTokens))
	}
	if len(parts) == 0 {
		return message
	}
	return message + " (" + strings.Join(parts, ", ") + ")"
}

// notifyOnce sends a message at most once per session: a thrashing session can
// produce the same reason on every turn, and /cc-compact reset is what makes the
// message reportable again.
func (ep *endpoint) notifyOnce(h hostCall, sessionID, message, level string) {
	key := sessionID + "|" + message
	ep.mu.Lock()
	seen := ep.notified[key]
	ep.notified[key] = true
	ep.mu.Unlock()
	if seen {
		return
	}
	h.Notify(message, level)
}

// forgetNotified drops every message remembered for a session, so a reset lets
// the next failure be reported again.
func (ep *endpoint) forgetNotified(sessionID string) {
	prefix := sessionID + "|"
	ep.mu.Lock()
	defer ep.mu.Unlock()
	delete(ep.notified, sessionID)
	for key := range ep.notified {
		if strings.HasPrefix(key, prefix) {
			delete(ep.notified, key)
		}
	}
}

// appendTrace writes one record straight to the trace sink. The endpoint uses it
// for facts it settles before the driver is built, and tracing stays best
// effort: a broken log never fails a compaction.
func (ep *endpoint) appendTrace(rec TraceRecord) {
	if ep.trace == nil {
		return
	}
	if rec.TS.IsZero() {
		rec.TS = now()
	}
	_ = ep.trace.Append(rec)
}

// --- events ------------------------------------------------------------------

// onSessionStart reports a plugin that did not load, once per session. Nothing
// else needs the event: the extension is otherwise quiet at startup.
func (ep *endpoint) onSessionStart(h hostCall, _ map[string]any) (any, error) {
	ep.notifyLoadFailure(h)
	return nil, nil
}

// notifyLoadFailure tells the user why compaction will not be taken over.
func (ep *endpoint) notifyLoadFailure(h hostCall) {
	if ep.loadErr == nil {
		return
	}
	ep.notifyOnce(h, sessionIDOf(h), extensionName+": "+ep.loadErr.Error(), notifyError)
}

// onBeforeProviderRequest captures the body the session is about to send. The
// host keeps its own payload: returning nil changes nothing, which is what the
// cache-prefix reuse depends on.
func (ep *endpoint) onBeforeProviderRequest(h hostCall, data map[string]any) (any, error) {
	payload, ok := decodePayload(data)
	if !ok {
		return nil, nil
	}
	sessionID := sessionIDOf(h)
	if sessionID == "" {
		return nil, nil
	}

	ep.newDriver(h).BeforeProviderRequest(sessionID, h.Model(), h.ModelProvider(), payload)
	return nil, nil
}

// onSessionBeforeCompact asks the policy whether to take compaction over and,
// when it does, returns PiG's compaction result. Every failure — an undecodable
// event, a missing cut point, a policy error, a stream error, a summary that
// parses to nothing — returns nil so PiG compacts the way it would have without
// the extension.
func (ep *endpoint) onSessionBeforeCompact(h hostCall, data map[string]any) (any, error) {
	ev, ok := decodeCompactEvent(data)
	if !ok {
		return nil, nil
	}
	sessionID := sessionIDOf(h)
	if sessionID == "" {
		return nil, nil
	}

	// PiG owns the cut point. A result with an empty firstKeptEntryId would
	// replace the session's compaction entry with an empty cut and corrupt it,
	// so hand compaction back before the policy or the driver can produce one.
	if ev.Preparation.FirstKeptEntryID == "" {
		ep.appendTrace(TraceRecord{
			SessionID: sessionID,
			Outcome:   OutcomeFallback,
			Detail:    noCutPointDetail,
		})
		ep.notifyOnce(h, sessionID,
			extensionName+": "+noCutPointDetail+"; PiG will compact this session itself",
			notifyWarning)
		return nil, nil
	}

	result := ep.newDriver(h).BeforeCompact(
		runContext(h), sessionID, h.Model(), h.ModelProvider(), ev)
	if result == nil {
		// A typed nil would reach the host as a result; the host must see no
		// result at all.
		return nil, nil
	}
	return result, nil
}

// onSessionShutdown drops the captured body: a closed session's payload must not
// outlive it in the process-wide table.
func (ep *endpoint) onSessionShutdown(h hostCall, _ map[string]any) (any, error) {
	sessionID := sessionIDOf(h)
	if sessionID == "" {
		return nil, nil
	}
	ep.state.Forget(sessionID)
	ep.forgetNotified(sessionID)
	return nil, nil
}

// --- /cc-compact -------------------------------------------------------------

// onCompactCommand is /cc-compact. A bare invocation or "status" reports the
// session, "reset" re-arms the breaker, and "now [instructions]" asks PiG for a
// compaction — with the rest of the line as Additional Instructions, which the
// design (§4 step 12) requires to reach the summary prompt.
func (ep *endpoint) onCompactCommand(h hostCall, args string) error {
	arg := strings.TrimSpace(args)

	switch {
	case arg == "" || arg == compactStatusArg:
		h.Notify(ep.statusReport(h), notifyInfo)
	case arg == compactResetArg:
		sessionID := sessionIDOf(h)
		ep.state.Forget(sessionID)
		ep.forgetNotified(sessionID)
		h.Notify(extensionName+": breaker re-armed and the captured request dropped for this session", notifyInfo)
	case arg == compactNowArg || strings.HasPrefix(arg, compactNowArg+" "):
		instructions := strings.TrimSpace(strings.TrimPrefix(arg, compactNowArg))
		opts := map[string]any{}
		if instructions != "" {
			opts["customInstructions"] = instructions
		}
		h.Compact(opts)
	default:
		h.Notify(compactUsage, notifyWarning)
	}
	return nil
}

// statusReport is the /cc-compact status text: enough for a user to see whether
// a session is being taken over, why not, and where the trace explains the rest.
func (ep *endpoint) statusReport(h hostCall) string {
	sessionID := sessionIDOf(h)
	lines := []string{extensionName + " status"}

	// The load error replaces the source: when the policy did not compile, the
	// error is the only useful answer to "where does this come from".
	if ep.loadErr != nil {
		lines = append(lines, "plugin: "+ep.loadErr.Error())
	} else {
		lines = append(lines, "plugin: "+pluginSource())
	}
	lines = append(lines, "trace: "+ep.traceFile)

	if captured, ok := ep.state.Get(sessionID); ok {
		lines = append(lines, fmt.Sprintf("capture: %s via %s, %d bytes, %s old",
			captured.ModelID, captured.Provider, len(captured.Payload),
			now().Sub(captured.At).Round(time.Second)))
	} else {
		lines = append(lines, "capture: none")
	}

	breaker := ep.state.Breaker(sessionID)
	lines = append(lines, fmt.Sprintf("breaker: fails=%d thrash=%d disabled=%t",
		breaker.Fails, breaker.Thrash, breaker.Disabled))
	lines = append(lines, ep.compactionReport(h)...)
	return strings.Join(lines, "\n")
}

// compactionReport is the settings half of the status text: the thresholds the
// host really uses, and the values that stop compaction before this extension is
// ever asked to take over.
func (ep *endpoint) compactionReport(h hostCall) []string {
	settings, err := h.Settings()
	if err != nil {
		return []string{"compaction: unavailable (" + err.Error() + ")"}
	}
	usage, usageErr := h.Usage()
	if usageErr != nil {
		usage = usageView{}
	}
	return settingsReport(settingsFromRaw(settings), usage)
}

// Pi's DEFAULT_COMPACTION_SETTINGS, used when the file leaves a value out.
const (
	defaultReserveTokens    = 16384
	defaultKeepRecentTokens = 20000
)

// settingsView is what the status line says about compaction thresholds.
type settingsView struct {
	Enabled          bool
	ReserveTokens    int
	KeepRecentTokens int
	Window           int
	Tokens           int
	TokensKnown      bool
}

func settingsFromRaw(raw map[string]any) settingsView {
	view := settingsView{
		Enabled:          true,
		ReserveTokens:    defaultReserveTokens,
		KeepRecentTokens: defaultKeepRecentTokens,
	}
	compaction, ok := raw["compaction"].(map[string]any)
	if !ok {
		return view
	}
	if enabled, ok := compaction["enabled"].(bool); ok {
		view.Enabled = enabled
	}
	if tokens := intOf(compaction["reserveTokens"]); tokens > 0 {
		view.ReserveTokens = tokens
	}
	if tokens := intOf(compaction["keepRecentTokens"]); tokens > 0 {
		view.KeepRecentTokens = tokens
	}
	return view
}

// settingsReport names the two thresholds that turn compaction off without
// saying so. PiG refuses a compaction whose cut point keeps the whole session,
// with "Nothing to compact (session too small)", and it never emits the event
// that would let this extension take over, so no trace line records it either.
func settingsReport(view settingsView, usage usageView) []string {
	lines := []string{fmt.Sprintf("compaction: enabled=%t reserve=%d keepRecent=%d window=%d",
		view.Enabled, view.ReserveTokens, view.KeepRecentTokens, usage.ContextWindow)}
	if !view.Enabled {
		return append(lines, "warning: compaction.enabled is false, so the session never compacts")
	}
	switch {
	case usage.Known && view.KeepRecentTokens >= usage.Tokens:
		lines = append(lines, fmt.Sprintf(
			"warning: keepRecent=%d is not below the %d tokens in this session, so PiG answers \"Nothing to compact (session too small)\" and this extension never runs; lower compaction.keepRecentTokens",
			view.KeepRecentTokens, usage.Tokens))
	case usage.ContextWindow > 0 && view.KeepRecentTokens*2 > usage.ContextWindow:
		lines = append(lines, fmt.Sprintf(
			"warning: keepRecent=%d is more than half the %d-token window: the cut lands inside a turn and nearly everything is kept, so compaction runs rarely and every run keeps the retained range unwieldy; lower compaction.keepRecentTokens",
			view.KeepRecentTokens, usage.ContextWindow))
	}
	return lines
}

// intOf reads a JSON number the way a decoded settings file leaves it.
func intOf(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0
		}
		return int(parsed)
	}
	return 0
}

// --- wire decoding -----------------------------------------------------------

// num coerces a decoded JSON number to an int. The host marshals the compaction
// event as JSON, so a count arrives as float64 from encoding/json and as
// json.Number when a decoder keeps the source text; both, plus every int and
// uint kind, are accepted. Anything else — a string, a missing key — is 0,
// because a malformed count must not lose the compaction.
func num(v any) int {
	return driverNumberInt(v)
}

// decodeCompactEvent decodes PiG's session_before_compact payload. The second
// result is false when there is no preparation to summarise, which is the only
// thing that makes the event unusable: a malformed optional field is decoded as
// its zero value instead.
func decodeCompactEvent(data map[string]any) (CompactEvent, bool) {
	prep, ok := decodePreparation(data["preparation"])
	if !ok {
		return CompactEvent{}, false
	}
	return CompactEvent{
		Reason:             stringField(data, "reason"),
		CustomInstructions: stringField(data, "customInstructions"),
		WillRetry:          boolField(data, "willRetry"),
		Preparation:        prep,
	}, true
}

// decodePreparation decodes preparation, the part of the event the policy reads.
// A missing, nil or non-object value means there is nothing to compact.
func decodePreparation(value any) (Preparation, bool) {
	obj, ok := value.(map[string]any)
	if !ok || obj == nil {
		return Preparation{}, false
	}
	return Preparation{
		FirstKeptEntryID: stringField(obj, "firstKeptEntryId"),
		TokensBefore:     num(obj["tokensBefore"]),
		PreviousSummary:  stringField(obj, "previousSummary"),
		IsSplitTurn:      boolField(obj, "isSplitTurn"),
		FileOps:          decodeFileOps(obj["fileOps"]),
		Settings:         decodeSettings(obj["settings"]),
		// Every field this decoder does not copy is a field the driver never
		// sees, and nothing fails when one goes missing.
		TurnPrefixMessages: rawMessages(obj["turnPrefixMessages"]),
	}, true
}

// rawMessages keeps messages exactly as the host sent them: the SDK serializer
// reads their own key order, so decoding them into a struct here would lose
// what the summary request is built from.
func rawMessages(value any) []json.RawMessage {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]json.RawMessage, 0, len(list))
	for _, item := range list {
		encoded, err := json.Marshal(item)
		if err != nil {
			continue
		}
		out = append(out, encoded)
	}
	return out
}

// decodeFileOps decodes the files the turn touched. A missing or foreign shape
// yields nil lists: the fields only inform the prompt.
func decodeFileOps(value any) FileOps {
	obj, ok := value.(map[string]any)
	if !ok {
		return FileOps{}
	}
	return FileOps{
		Read:    stringSlice(obj["read"]),
		Written: stringSlice(obj["written"]),
		Edited:  stringSlice(obj["edited"]),
	}
}

// decodeSettings decodes the compaction thresholds the host is running under.
func decodeSettings(value any) Settings {
	obj, ok := value.(map[string]any)
	if !ok {
		return Settings{}
	}
	return Settings{
		Enabled:          boolField(obj, "enabled"),
		ReserveTokens:    num(obj["reserveTokens"]),
		KeepRecentTokens: num(obj["keepRecentTokens"]),
	}
}

// decodePayload extracts the provider request body from PiG's
// before_provider_request envelope. The body is returned as it arrived — the
// capture is resent verbatim, so re-encoding it here would risk changing it —
// and an absent payload means there is nothing to capture.
func decodePayload(data map[string]any) (any, bool) {
	payload, ok := data["payload"]
	if !ok || payload == nil {
		return nil, false
	}
	return payload, true
}

// stringField reads a string value, tolerating its absence or a foreign type.
func stringField(obj map[string]any, key string) string {
	value, _ := obj[key].(string)
	return value
}

// boolField reads a bool value, tolerating its absence or a foreign type.
func boolField(obj map[string]any, key string) bool {
	value, _ := obj[key].(bool)
	return value
}

// stringSlice reads a JSON string array. The host decodes arrays as []any, but
// a caller that built the event in Go may pass []string, and both are accepted.
func stringSlice(value any) []string {
	switch items := value.(type) {
	case []string:
		if len(items) == 0 {
			return nil
		}
		return append([]string(nil), items...)
	case []any:
		out := make([]string, 0, len(items))
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				continue
			}
			out = append(out, text)
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}

// tracePath is where the trace log goes: PIG_CC_TRACE when set, and the
// per-user agent directory otherwise, next to the other PiG logs. The user
// directory comes from os.UserHomeDir, so an unset HOME cannot quietly drop the
// trace next to the working directory.
func tracePath() string {
	if path := os.Getenv(traceEnv); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".pi", "agent", "pi-cc-trace.jsonl")
}
