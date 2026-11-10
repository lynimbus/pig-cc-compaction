package pigcc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// StreamFunc sends one model request and returns the provider response. The
// host calls onPayload with the body it was about to send; returning a
// replacement makes the host send that body instead.
type StreamFunc func(model map[string]any, request map[string]any, onPayload func(any, map[string]any) (any, error)) (map[string]any, error)

// FindModelFunc resolves the provider/model pair the compaction request must be
// streamed through.
type FindModelFunc func(provider, modelID string) (map[string]any, bool)

// DriverOptions wires the driver to the host. Plugin and State are required;
// every other field has a safe zero value.
type DriverOptions struct {
	Plugin    Plugin
	State     State
	Trace     Sink
	Stream    StreamFunc
	FindModel FindModelFunc
	Now       func() time.Time
	Sleep     func(ctx context.Context, d time.Duration) error
}

// Driver turns a captured provider payload into a resent summarization request
// and folds the parsed <summary> back into PiG's compaction result.
type Driver struct {
	plugin    Plugin
	state     State
	trace     Sink
	stream    StreamFunc
	findModel FindModelFunc
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration) error
}

// NewDriver builds a Driver, filling optional dependencies with their defaults.
// A nil Plugin is not an error: it is the policy-unavailable case the extension
// reports once at session start, and the driver then declines every compaction so
// the host compacts its own way. Nothing here panics.
func NewDriver(opts DriverOptions) *Driver {
	if opts.Plugin == nil {
		opts.Plugin = decliningPlugin{}
	}
	if opts.State == nil {
		opts.State = NewState()
	}

	d := &Driver{
		plugin:    opts.Plugin,
		state:     opts.State,
		trace:     opts.Trace,
		stream:    opts.Stream,
		findModel: opts.FindModel,
		now:       opts.Now,
		sleep:     opts.Sleep,
	}
	if d.now == nil {
		d.now = time.Now
	}
	if d.sleep == nil {
		d.sleep = driverSleepWithContext
	}
	return d
}

// decliningPlugin stands in for a policy that never loaded. Every call declines,
// which produces the same outcome as an extension that returns nothing, so a
// broken policy file costs the feature and not the session.
type decliningPlugin struct{}

// Plan declines with the reason a trace reader needs to tell a missing policy
// from a policy that decided not to take over.
func (decliningPlugin) Plan(context.Context, PlanInput) (PlanOutput, error) {
	return PlanOutput{Action: "pass", Why: "no policy loaded: the extension reported the load error at session start"}, nil
}

// Parse is never reached while Plan declines; it declines for the same reason.
func (decliningPlugin) Parse(context.Context, ParseInput) (ParseOutput, error) {
	return ParseOutput{None: true, Why: "no policy loaded"}, nil
}

// BeforeProviderRequest keeps the payload verbatim. It always returns nil so
// PiG sends its own body unchanged; replacing it here would break the cache
// prefix the compaction request relies on.
// summarizationMarker is the closing line of the compaction prompt. A request
// whose last turn carries it is this extension's own summarization call, and
// capturing that would overwrite the very prefix it was built from: the next
// attempt would then append a second instruction to the first one.
const summarizationMarker = "Your response must contain only the <analysis> and <summary> blocks."

func (d *Driver) BeforeProviderRequest(sessionID, modelID, provider string, payload any) any {
	if payload == nil {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	if isSummarizationRequest(raw) {
		return nil
	}
	d.state.Put(sessionID, modelID, provider, raw)
	return nil
}

// isSummarizationRequest reports whether a provider body ends with the compaction
// instruction. Only the last turn is inspected: the instruction is always the
// final user message of the request this driver sends.
func isSummarizationRequest(raw json.RawMessage) bool {
	var body struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || len(body.Messages) == 0 {
		return false
	}
	last := body.Messages[len(body.Messages)-1].Content
	var text string
	if err := json.Unmarshal(last, &text); err == nil {
		return isSummarizationText(text)
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(last, &blocks); err != nil {
		return false
	}
	for _, block := range blocks {
		if isSummarizationText(block.Text) {
			return true
		}
	}
	return false
}

// isSummarizationText reports whether a prompt is one this driver wrote. Every
// request it sends has to be recognizable here: capturing one would overwrite
// the prefix it was built from, and the next attempt would then append a second
// instruction to the first.
func isSummarizationText(text string) bool {
	return strings.Contains(text, summarizationMarker) || strings.Contains(text, turnPrefixMarker)
}

// BeforeCompact asks the policy whether to take over, resends the captured
// provider payload at most twice, and returns PiG's compaction result. Every
// bail-out returns nil, which hands compaction back to PiG.
func (d *Driver) BeforeCompact(ctx context.Context, sessionID, modelID, provider string, ev CompactEvent) map[string]any {
	start := d.now()

	var captureIn *CaptureIn
	if captured, ok := d.state.Get(sessionID); ok {
		captureIn = &CaptureIn{
			Payload:  captured.Payload,
			ModelID:  captured.ModelID,
			Provider: captured.Provider,
			AgeMs:    start.Sub(captured.At).Milliseconds(),
		}
	}

	model, found := d.lookupModel(provider, modelID)

	// lastRes/lastText keep the final provider answer so a run that never yields
	// a summary can say why: the provider's own error string is the only place a
	// rejected resend explains itself.
	lastRes := map[string]any{}
	var lastText string

	for attempt := 0; attempt < 2; attempt++ {
		in := PlanInput{
			Reason:             ev.Reason,
			Attempt:            attempt,
			CustomInstructions: ev.CustomInstructions,
			Capture:            captureIn,
			Breaker:            d.state.Breaker(sessionID),
			Preparation: PrepIn{
				FirstKeptEntryID: ev.Preparation.FirstKeptEntryID,
				TokensBefore:     ev.Preparation.TokensBefore,
				PreviousSummary:  ev.Preparation.PreviousSummary,
			},
		}

		plan, err := d.plugin.Plan(ctx, in)
		if err != nil {
			d.state.NoteFailure(sessionID)
			d.record(start, sessionID, OutcomeFailed, err.Error(), nil)
			return nil
		}
		if plan.Action != "takeover" {
			d.record(start, sessionID, OutcomeFallback, plan.Why, nil)
			return nil
		}
		if !found {
			d.state.NoteFailure(sessionID)
			d.record(start, sessionID, OutcomeFailed, "model not found: "+provider+"/"+modelID, nil)
			return nil
		}

		res, err := d.streamWithRetry(ctx, model, plan.Request, plan.Payload)
		if err != nil {
			d.state.NoteFailure(sessionID)
			d.record(start, sessionID, OutcomeFailed, err.Error(), nil)
			return nil
		}

		text := textBlocks(res["content"])
		lastRes = res
		lastText = text
		stop := stopReason(res)
		out := usageInt(res, "output")

		parsed, err := d.plugin.Parse(ctx, ParseInput{
			Text:         text,
			StopReason:   stop,
			OutputTokens: out,
			Attempt:      attempt,
		})
		if err != nil {
			d.state.NoteFailure(sessionID)
			d.record(start, sessionID, OutcomeFailed, err.Error(), nil)
			return nil
		}
		if parsed.Summary != "" {
			d.state.NoteSuccess(sessionID)
			// A cut that is not a turn start leaves the retained range in the
			// middle of a turn, so Pi appends a checkpoint of how that turn
			// began. The checkpoint is auxiliary: the nine sections already
			// cover the whole conversation, so a call that fails, comes back
			// empty or is cut off appends nothing and says so in the trace.
			marker, note := "", ""
			switch {
			case !ev.Preparation.IsSplitTurn:
				// The cut starts a turn, so the retained range needs no
				// checkpoint in front of it.
			case len(ev.Preparation.TurnPrefixMessages) == 0:
				note = "split turn: the preparation carried no turn prefix messages"
			case captureIn == nil:
				note = "split turn: no captured request to reuse for the checkpoint"
			default:
				marker, note = d.turnContext(ctx, model, captureIn.Payload, ev)
			}
			d.record(start, sessionID, OutcomeCompacted, note, res)
			// The file list goes into the summary text, not only into details:
			// PiG seeds a compaction's file operations from the previous entry
			// only when that entry did not come from a hook, so the summary is
			// what carries the list into the next compaction. Pi appends it
			// after the split-turn checkpoint, and so does this.
			block, readFiles, modifiedFiles := fileOperationsBlock(ev.Preparation.FileOps, ev.Preparation.PreviousSummary)
			compaction := map[string]any{
				"summary":          parsed.Summary + marker + block,
				"firstKeptEntryId": ev.Preparation.FirstKeptEntryID,
				"tokensBefore":     ev.Preparation.TokensBefore,
			}
			if len(readFiles) > 0 || len(modifiedFiles) > 0 {
				compaction["details"] = map[string]any{
					"readFiles":     readFiles,
					"modifiedFiles": modifiedFiles,
				}
			}
			return map[string]any{"compaction": compaction}
		}
		if !parsed.Retry {
			// The policy layer sees the text and the stop reason, never the host's
			// error string, so fold it in here: a rejected resend explains itself
			// through nothing else.
			detail := parsed.Why
			if message := providerError(res); message != "" {
				detail += " (errorMessage=" + message + ")"
			}
			// The policy says why it rejected the reply but never sees the text,
			// so the opening of the reply lands here: it is the difference
			// between "wrote prose instead of tags" and "answered something else".
			if head := snippet(text); head != "" {
				detail += " (text=" + head + ")"
			}
			d.state.NoteFailure(sessionID)
			d.record(start, sessionID, OutcomeFallback, detail, res)
			return nil
		}
		// The reply was rejected while another attempt is left. It gets its own
		// outcome so a retry that succeeds does not hide why the first reply was
		// unusable, which is the difference between a truncation and a policy
		// that is too strict.
		d.record(start, sessionID, OutcomeRetry, parsed.Why, res)
	}

	d.state.NoteFailure(sessionID)
	d.record(start, sessionID, OutcomeFallback, "no summary after two attempts: "+providerNote(lastRes, lastText), lastRes)
	return nil
}

// turnContext summarizes the beginning of the turn the cut point falls into,
// the way Pi does when a cut is not a turn start, and returns the text to append
// plus a note explaining a skipped checkpoint. Both or neither: a marker with no
// checkpoint behind it would claim context it does not contain.
func (d *Driver) turnContext(ctx context.Context, model map[string]any, captured json.RawMessage, ev CompactEvent) (string, string) {
	prefix := fitTurnPrefix(ev.Preparation.TurnPrefixMessages, contextWindowOf(model), maxOutputOf(model))
	prompt, err := summarizeTurnPrefixPrompt(prefix, ev.CustomInstructions)
	if err != nil {
		return "", "split turn: the prefix messages do not serialize (" + err.Error() + ")"
	}
	payload, err := turnPrefixPayload(captured, prompt)
	if err != nil {
		return "", "split turn: the captured body cannot be reused (" + err.Error() + ")"
	}
	res, err := d.streamWithRetry(ctx, model, turnPrefixRequest(prompt), payload)
	if err != nil {
		return "", "split turn: the prefix request failed (" + err.Error() + ")"
	}
	text := strings.TrimSpace(textBlocks(res["content"]))
	if text == "" {
		return "", "split turn: the prefix reply was empty (stop=" + stopReason(res) + ")"
	}
	if !cleanStop(stopReason(res)) {
		return "", "split turn: the prefix reply did not finish (stop=" + stopReason(res) + ")"
	}
	return splitTurnSeparator + text, ""
}

// providerError returns the host's own error string when a stream failed
// before or during the provider call.
func providerError(res map[string]any) string {
	if res == nil {
		return ""
	}
	message, _ := res["errorMessage"].(string)
	return message
}

// providerNote explains a terminal provider answer in one line. The provider's
// error string comes first because it is the only text a refused resend gives.
func providerNote(res map[string]any, text string) string {
	var parts []string
	if res != nil {
		if message, ok := res["errorMessage"].(string); ok && message != "" {
			parts = append(parts, "errorMessage="+message)
		}
	}
	parts = append(parts, "stopReason="+stopReason(res), fmt.Sprintf("outputTokens=%d", usageInt(res, "output")))
	if head := snippet(text); head != "" {
		parts = append(parts, "text="+head)
	}
	return strings.Join(parts, " ")
}

// snippetLen caps the reply text a trace line carries. It is short on purpose:
// enough to tell prose from a truncated tag, not a copy of the conversation.
const snippetLen = 240

// snippet returns the opening of a reply for one trace line: whitespace
// collapsed so the line stays one line, cut on a rune boundary, empty when the
// reply had no text.
func snippet(text string) string {
	head := strings.Join(strings.Fields(text), " ")
	if head == "" {
		return ""
	}
	if len(head) <= snippetLen {
		return head
	}
	cut := snippetLen
	for cut > 0 && !utf8.RuneStart(head[cut]) {
		cut--
	}
	return head[:cut] + "…"
}

// streamWithRetry tries the replacement request twice at most. The first
// failure is followed by a 250ms pause; a cancelled context ends the loop
// immediately and no further attempt is made.
func (d *Driver) streamWithRetry(ctx context.Context, model map[string]any, request, payload json.RawMessage) (map[string]any, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		res, err := d.streamOnce(ctx, model, request, payload)
		if err == nil {
			return res, nil
		}
		lastErr = err

		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if attempt == 0 {
			if err := d.sleep(ctx, 250*time.Millisecond); err != nil {
				return nil, err
			}
		}
	}
	if lastErr == nil {
		lastErr = errors.New("pigcc: stream failed")
	}
	return nil, lastErr
}

// streamOnce invokes the host stream without letting a stream that ignores its
// context pin the compaction hook forever.
func (d *Driver) streamOnce(ctx context.Context, model map[string]any, request, payload json.RawMessage) (map[string]any, error) {
	if d.stream == nil {
		return nil, errors.New("pigcc: no Stream configured")
	}

	decodedRequest, err := decodeDriverObject(request)
	if err != nil {
		return nil, fmt.Errorf("pigcc: decode plan request: %w", err)
	}

	onPayload := func(any, map[string]any) (any, error) {
		if len(payload) == 0 {
			return nil, nil
		}
		var replacement any
		if err := json.Unmarshal(payload, &replacement); err != nil {
			return nil, fmt.Errorf("pigcc: decode plan payload: %w", err)
		}
		return replacement, nil
	}

	type result struct {
		res map[string]any
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := d.stream(model, decodedRequest, onPayload)
		done <- result{res: res, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-done:
		// Cancellation wins even when the stream happened to finish at the
		// same time.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return out.res, out.err
	}
}

func (d *Driver) lookupModel(provider, modelID string) (map[string]any, bool) {
	if d.findModel == nil {
		return nil, false
	}
	return d.findModel(provider, modelID)
}

func (d *Driver) record(start time.Time, sessionID, outcome, detail string, res map[string]any) {
	if d.trace == nil {
		return
	}
	rec := TraceRecord{
		TS:         start,
		SessionID:  sessionID,
		Outcome:    outcome,
		Detail:     detail,
		DurationMs: d.now().Sub(start).Milliseconds(),
	}
	if res != nil {
		rec.InputTokens = usageInt(res, "input")
		rec.CacheReadTokens = usageInt(res, "cacheRead")
		rec.OutputTokens = usageInt(res, "output")
	}
	// Tracing is best effort: a broken trace log must never fail compaction.
	_ = d.trace.Append(rec)
}

func driverSleepWithContext(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func decodeDriverObject(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]any)
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// textBlocks concatenates the text blocks of a provider content array. Blocks
// of any other shape are skipped rather than treated as an error.
func textBlocks(value any) string {
	var parts []string
	switch blocks := value.(type) {
	case []any:
		for _, block := range blocks {
			if text, ok := driverTextBlock(block); ok {
				parts = append(parts, text)
			}
		}
	case []map[string]any:
		for _, block := range blocks {
			if text, ok := driverTextBlock(block); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func driverTextBlock(value any) (string, bool) {
	block, ok := value.(map[string]any)
	if !ok {
		return "", false
	}
	kind, _ := block["type"].(string)
	if kind != "text" {
		return "", false
	}
	text, ok := block["text"].(string)
	if !ok {
		return "", false
	}
	return text, true
}

// usageInt reads a numeric field from res.usage, tolerating every numeric type
// encoding/json can produce.
func usageInt(res map[string]any, key string) int {
	if res == nil {
		return 0
	}
	usage, ok := res["usage"].(map[string]any)
	if !ok {
		return 0
	}
	return driverNumberInt(usage[key])
}

func driverNumberInt(value any) int {
	switch n := value.(type) {
	case int:
		return n
	case int8:
		return int(n)
	case int16:
		return int(n)
	case int32:
		return int(n)
	case int64:
		return int(n)
	case uint:
		return int(n)
	case uint8:
		return int(n)
	case uint16:
		return int(n)
	case uint32:
		return int(n)
	case uint64:
		return int(n)
	case float32:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
		if f, err := n.Float64(); err == nil {
			return int(f)
		}
	}
	return 0
}

func stopReason(res map[string]any) string {
	if res == nil {
		return ""
	}
	reason, _ := res["stopReason"].(string)
	return reason
}
