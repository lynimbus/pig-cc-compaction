package pigcc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moejs"
)

// The plugin the package ships must satisfy the contract interface.
var _ Plugin = (*MoePlugin)(nil)

// takeoverPayload is a provider request shaped like the one Pi sends: a model
// id with a provider prefix, a system prompt and tools with cache markers, and
// one user message. Takeover must resend it field for field, or the provider's
// prompt cache misses and the whole point of the design is lost.
const takeoverPayload = `{"model":"group/deepseek","max_tokens":8192,"temperature":0.2,` +
	`"stream":true,"system":[{"type":"text","text":"be brief","cache_control":{"type":"ephemeral"}}],` +
	`"tools":[{"name":"bash","description":"run shell commands","cache_control":{"type":"ephemeral"}}],` +
	`"messages":[{"role":"user","content":[{"type":"text","text":"deploy the service"}]}]}`

// turn is one entry of the messages array, its content kept raw because a
// provider message carries either a string or an array of content blocks.
type turn struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func mustPlugin(t *testing.T) *MoePlugin {
	t.Helper()
	p, err := NewPlugin("compaction.js", DefaultSource())
	if err != nil {
		t.Fatalf("NewPlugin(DefaultSource()): %v", err)
	}
	return p
}

func mustPluginFrom(t *testing.T, src string) *MoePlugin {
	t.Helper()
	p, err := NewPlugin("test.js", src)
	if err != nil {
		t.Fatalf("NewPlugin(test source): %v", err)
	}
	return p
}

// captured builds the capture a session would have recorded.
func captured(payload string) *CaptureIn {
	if payload == "" {
		return nil
	}
	return &CaptureIn{
		Payload:  json.RawMessage(payload),
		ModelID:  "group/deepseek",
		Provider: "deepseek",
		AgeMs:    1200,
	}
}

// planInput is the event a normal compaction call hands to the policy.
func planInput(reason string) PlanInput {
	return PlanInput{
		Reason:             reason,
		Attempt:            0,
		CustomInstructions: "",
		Capture:            captured(takeoverPayload),
		Breaker:            Breaker{},
		Preparation: PrepIn{
			FirstKeptEntryID: "entry-42",
			TokensBefore:     120000,
			PreviousSummary:  "an earlier summary",
		},
	}
}

func canon(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("empty JSON")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("json.Compact(%q): %v", raw, err)
	}
	return buf.String()
}

// messagesOf decodes a {"messages": [...]} request into its turns.
func messagesOf(t *testing.T, raw json.RawMessage) []turn {
	t.Helper()
	var wire struct {
		Messages []turn `json:"messages"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode request %q: %v", raw, err)
	}
	if wire.Messages == nil {
		t.Fatalf("request %q has no messages array", raw)
	}
	return wire.Messages
}

// instructionOf returns the text of the single text block of the last turn,
// which is where the summary instruction lands.
func instructionOf(t *testing.T, msgs []turn) string {
	t.Helper()
	if len(msgs) == 0 {
		t.Fatal("no messages")
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		t.Fatalf("last message role = %q, want user", last.Role)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(last.Content, &blocks); err != nil {
		t.Fatalf("last message content %q is not a content block array: %v", last.Content, err)
	}
	if len(blocks) != 1 {
		t.Fatalf("last message has %d content blocks, want exactly 1: %q", len(blocks), last.Content)
	}
	if blocks[0].Type != "text" {
		t.Fatalf("last message block type = %q, want text", blocks[0].Type)
	}
	if blocks[0].Text == "" {
		t.Fatal("the summary instruction is empty")
	}
	return blocks[0].Text
}

// takeover runs a plan that must take over and returns its payload and request
// messages.
func takeover(t *testing.T, p *MoePlugin, in PlanInput) (json.RawMessage, []turn) {
	t.Helper()
	out, err := p.Plan(t.Context(), in)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if out.Action != "takeover" {
		t.Fatalf("action = %q (why %q), want takeover", out.Action, out.Why)
	}
	return out.Payload, messagesOf(t, out.Request)
}

// TestDefaultSourceIsTheEmbeddedPlugin: DefaultSource serves plugin/compaction.js
// and the CC prompt is embedded verbatim in it, so a wording change is a plugin
// edit, never a Go one.
func TestDefaultSourceIsTheEmbeddedPlugin(t *testing.T) {
	src := DefaultSource()
	if src == "" {
		t.Fatal("DefaultSource() is empty")
	}
	for _, want := range []string{
		"export function planCompact",
		"export function parseSummary",
		"Your task is to create a detailed summary of the conversation so far",
		"2. Double-check for technical accuracy and completeness",
		"9. Optional Next Step:",
		"Your response must contain only the <analysis> and <summary> blocks.",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("plugin source does not contain %q", want)
		}
	}
	// The plugin must stay a self-contained ES module: no imports, no Node.
	if re := regexp.MustCompile(`(?m)^\s*import\b`); re.MatchString(src) {
		t.Error("plugin source has an import statement")
	}
	for _, banned := range []string{"require(", "process.", "Buffer.", "__dirname", "fs."} {
		if strings.Contains(src, banned) {
			t.Errorf("plugin source uses a Node API: %q", banned)
		}
	}
}

// TestNewPluginRejectsBadSources: a broken module or a module without the two
// hooks is an error, not a panic.
func TestNewPluginRejectsBadSources(t *testing.T) {
	if p, err := NewPlugin("broken.js", "export function planCompact( {"); err == nil {
		t.Errorf("NewPlugin accepted a module that does not compile: %+v", p)
	}
	if p, err := NewPlugin("nohooks.js", "export const helper = 1;"); err == nil {
		t.Errorf("NewPlugin accepted a module without planCompact/parseSummary: %+v", p)
	}
}

// TestPlanPassOnUnusableRequests covers the four pass rules, in order: overflow,
// no capture, open breaker, no messages in the captured payload.
func TestPlanPassOnUnusableRequests(t *testing.T) {
	p := mustPlugin(t)

	cases := []struct {
		name string
		in   func() PlanInput
		// why is a lowercase substring the reason must contain; empty means
		// only "non-empty" is required.
		why string
	}{
		{
			name: "overflow",
			in:   func() PlanInput { return planInput("overflow") },
			why:  "overflow",
		},
		{
			name: "overflow wins over an open breaker",
			in: func() PlanInput {
				in := planInput("overflow")
				in.Breaker = Breaker{Fails: 3, Thrash: 2, Disabled: true}
				return in
			},
			why: "overflow",
		},
		{
			name: "no captured request",
			in: func() PlanInput {
				in := planInput("manual")
				in.Capture = nil
				return in
			},
			why: "captur",
		},
		{
			name: "no capture wins over manual",
			in: func() PlanInput {
				in := planInput("manual")
				in.Capture = nil
				in.Breaker = Breaker{Disabled: true}
				return in
			},
			why: "captur",
		},
		{
			// The breaker gates automatic compaction; "manual" is exempted,
			// see TestPlanManualCompactionBypassesBreaker.
			name: "circuit breaker open",
			in: func() PlanInput {
				in := planInput("threshold")
				in.Breaker = Breaker{Fails: 3, Thrash: 2, Disabled: true}
				return in
			},
			why: "circuit breaker open",
		},
		{
			name: "payload has no messages",
			in: func() PlanInput {
				in := planInput("manual")
				in.Capture = captured(`{"model":"group/deepseek","system":[{"type":"text","text":"be brief"}]}`)
				return in
			},
			why: "message",
		},
		{
			name: "messages is not an array",
			in: func() PlanInput {
				in := planInput("manual")
				in.Capture = captured(`{"model":"group/deepseek","messages":"deploy the service"}`)
				return in
			},
		},
		{
			name: "messages is empty",
			in: func() PlanInput {
				in := planInput("manual")
				in.Capture = captured(`{"model":"group/deepseek","messages":[]}`)
				return in
			},
		},
		{
			name: "payload is null",
			in: func() PlanInput {
				in := planInput("manual")
				in.Capture = &CaptureIn{ModelID: "group/deepseek", Provider: "deepseek"}
				return in
			},
		},
		{
			name: "payload is not an object",
			in: func() PlanInput {
				in := planInput("manual")
				in.Capture = captured(`"a string"`)
				return in
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := p.Plan(t.Context(), tc.in())
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if out.Action != "pass" {
				t.Fatalf("action = %q, want pass", out.Action)
			}
			if strings.TrimSpace(out.Why) == "" {
				t.Fatal("pass has an empty why")
			}
			if tc.why != "" && !strings.Contains(strings.ToLower(out.Why), tc.why) {
				t.Fatalf("why = %q, want it to mention %q", out.Why, tc.why)
			}
			if len(out.Payload) != 0 {
				t.Errorf("pass carries a replacement payload: %s", out.Payload)
			}
			if len(out.Request) != 0 {
				t.Errorf("pass carries a request: %s", out.Request)
			}
		})
	}
}

// TestPlanManualCompactionBypassesBreaker pins what the breaker may and may not
// stop: three failures disable AUTOMATIC takeover for the session, while an
// explicit /compact still takes over, because that is the only way to recover a
// tripped session from inside it (pig-cc-compaction.md §4.6). The other pass
// rules keep their priority over manual.
func TestPlanManualCompactionBypassesBreaker(t *testing.T) {
	p := mustPlugin(t)
	tripped := Breaker{Fails: 3, Thrash: 2, Disabled: true}

	t.Run("automatic compaction is blocked", func(t *testing.T) {
		in := planInput("threshold")
		in.Breaker = tripped
		out, err := p.Plan(t.Context(), in)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if out.Action != "pass" {
			t.Fatalf("action = %q, want pass", out.Action)
		}
		if !strings.Contains(out.Why, "circuit breaker open") {
			t.Fatalf("why = %q, want it to contain %q", out.Why, "circuit breaker open")
		}
	})

	t.Run("manual still takes over", func(t *testing.T) {
		in := planInput("manual")
		in.Breaker = tripped

		payload, requestMsgs := takeover(t, p, in)
		// The vehicle request carries the instruction alone: the host rejects the
		// provider-format "developer" role the captured body starts with.
		if len(requestMsgs) != 1 {
			t.Fatalf("request has %d messages, want the instruction alone", len(requestMsgs))
		}
		instructionOf(t, requestMsgs)
		if got := len(payloadMessages(t, payload)); got != 2 {
			t.Fatalf("payload has %d messages, want the captured one plus the instruction", got)
		}

		var want, got map[string]json.RawMessage
		if err := json.Unmarshal([]byte(takeoverPayload), &want); err != nil {
			t.Fatalf("decode the fixture: %v", err)
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("decode the replacement payload %q: %v", payload, err)
		}
		for k, v := range want {
			if k == "messages" {
				continue
			}
			if canon(t, got[k]) != canon(t, v) {
				t.Errorf("field %q = %s, want %s", k, got[k], v)
			}
		}
		if string(got["model"]) != `"group/deepseek"` {
			t.Errorf("model = %s, want \"group/deepseek\"", got["model"])
		}
	})

	t.Run("manual without a capture still passes", func(t *testing.T) {
		in := planInput("manual")
		in.Breaker = tripped
		in.Capture = nil
		out, err := p.Plan(t.Context(), in)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if out.Action != "pass" {
			t.Fatalf("action = %q, want pass", out.Action)
		}
		if !strings.Contains(strings.ToLower(out.Why), "captur") {
			t.Fatalf("why = %q, want it to mention the missing capture", out.Why)
		}
	})

	t.Run("overflow still wins over manual", func(t *testing.T) {
		in := planInput("overflow")
		in.Breaker = tripped
		out, err := p.Plan(t.Context(), in)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if out.Action != "pass" {
			t.Fatalf("action = %q, want pass", out.Action)
		}
		if !strings.Contains(strings.ToLower(out.Why), "overflow") {
			t.Fatalf("why = %q, want it to mention overflow", out.Why)
		}
	})
}

// TestPlanTakeoverKeepsThePayload: takeover resends the captured payload with
// one instruction appended and every other field untouched, which is the only
// reason the provider's prompt cache still hits.
func TestPlanTakeoverKeepsThePayload(t *testing.T) {
	p := mustPlugin(t)
	in := planInput("manual")

	payload, msgs := takeover(t, p, in)

	// Every field but messages is byte-identical to the capture.
	var want map[string]json.RawMessage
	if err := json.Unmarshal([]byte(takeoverPayload), &want); err != nil {
		t.Fatalf("decode the fixture: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("decode the replacement payload %q: %v", payload, err)
	}
	if len(got) != len(want) {
		t.Fatalf("replacement payload has %d fields, want %d: %s", len(got), len(want), payload)
	}
	for k, v := range want {
		if k == "messages" {
			continue
		}
		g, ok := got[k]
		if !ok {
			t.Fatalf("replacement payload dropped field %q: %s", k, payload)
		}
		if canon(t, g) != canon(t, v) {
			t.Errorf("field %q = %s, want %s", k, g, v)
		}
	}
	if model := string(got["model"]); !strings.Contains(model, "group/deepseek") {
		t.Errorf("model = %s, want the provider-prefixed group/deepseek", model)
	}

	// One message appended, the original one intact.
	var original []turn
	if err := json.Unmarshal(want["messages"], &original); err != nil {
		t.Fatalf("decode the fixture messages: %v", err)
	}
	if got := len(payloadMessages(t, payload)); got != len(original)+1 {
		t.Fatalf("payload has %d messages, want %d", got, len(original)+1)
	}
	if canon(t, payloadMessages(t, payload)[0].Content) != canon(t, original[0].Content) {
		t.Error("the captured message was modified")
	}
	instructionOf(t, msgs)

	// The vehicle request carries the instruction alone; the payload carries it as
	// the final turn, which is the body onPayload hands to the provider.
	if len(msgs) != 1 {
		t.Fatalf("request has %d messages, want the instruction alone", len(msgs))
	}
	carried := payloadMessages(t, payload)
	if canon(t, mustMarshal(t, msgs[0])) != canon(t, mustMarshal(t, carried[len(carried)-1])) {
		t.Error("the request does not carry the same instruction turn as the payload")
	}

	// The capture the host handed in is not modified.
	if string(in.Capture.Payload) != takeoverPayload {
		t.Error("Plan modified the caller's capture payload")
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// payloadMessages returns the messages of a replacement payload.
func payloadMessages(t *testing.T, payload json.RawMessage) []turn {
	t.Helper()
	var wire struct {
		Messages []turn `json:"messages"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		t.Fatalf("decode the replacement payload messages: %v", err)
	}
	return wire.Messages
}

// TestPlanCustomInstructions: a /compact argument is spliced verbatim into the
// instruction; without one the whole "Additional Instructions" block is gone.
func TestPlanCustomInstructions(t *testing.T) {
	p := mustPlugin(t)
	const custom = "只保留部署相关的部分"

	withCustom := planInput("manual")
	withCustom.CustomInstructions = custom
	_, msgs := takeover(t, p, withCustom)
	instr := instructionOf(t, msgs)
	if !strings.Contains(instr, custom) {
		t.Errorf("instruction does not carry the custom instructions verbatim:\n%s", instr)
	}
	if !strings.Contains(instr, "Additional Instructions") {
		t.Errorf("instruction has no Additional Instructions block:\n%s", instr)
	}

	empty := planInput("manual")
	_, msgs = takeover(t, p, empty)
	plain := instructionOf(t, msgs)
	if strings.Contains(plain, "Additional Instructions") {
		t.Errorf("instruction keeps an Additional Instructions block without custom instructions:\n%s", plain)
	}
	if strings.Contains(plain, "omit this whole block when empty") {
		t.Errorf("instruction keeps the placeholder text:\n%s", plain)
	}

	// The rest of the prompt is the same either way.
	if !strings.Contains(plain, "9. Optional Next Step:") {
		t.Error("the instruction lost the prompt body")
	}
	if !strings.Contains(plain, "Your response must contain only the <analysis> and <summary> blocks.") {
		t.Error("the instruction lost the output constraint")
	}
}

// TestPlanAttemptZeroVersusOne: a retry speaks in a stricter voice and says why.
func TestPlanAttemptZeroVersusOne(t *testing.T) {
	p := mustPlugin(t)

	first := planInput("manual")
	first.Attempt = 0
	_, msgs := takeover(t, p, first)
	instr0 := instructionOf(t, msgs)

	retry := planInput("manual")
	retry.Attempt = 1
	_, msgs = takeover(t, p, retry)
	instr1 := instructionOf(t, msgs)

	if instr0 == instr1 {
		t.Fatal("attempt 1 uses the very same instruction as attempt 0")
	}
	lower := strings.ToLower(instr1)
	if !strings.Contains(lower, "did not contain a usable <summary> block") {
		t.Errorf("the retry instruction does not say the previous reply lacked the summary block:\n%s", instr1)
	}
	if !strings.Contains(lower, "nine sections") {
		t.Errorf("the retry instruction does not ask for the nine sections:\n%s", instr1)
	}
	for _, want := range []string{"<analysis>", "<summary>"} {
		if !strings.Contains(instr1, want) {
			t.Errorf("the retry instruction does not name the %s block:\n%s", want, instr1)
		}
	}
	if !strings.Contains(instr1, "9. Optional Next Step:") {
		t.Error("the retry instruction lost the prompt body")
	}
	// A retry is only louder than the first attempt, never different in content.
	if !strings.Contains(instr1, "5. Problem Solving:") {
		t.Error("the retry instruction lost the prompt body")
	}
}

// TestPlanCompactReportsPromptChars: planCompact reports the length of the
// instruction it appended, for the trace log. PlanOutput has no such field, so
// this reads the JSON the plugin returns.
func TestPlanCompactReportsPromptChars(t *testing.T) {
	mod, err := moejs.Compile("compaction.js", DefaultSource())
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	hook, err := mod.Hook("planCompact")
	if err != nil {
		t.Fatalf("Hook: %v", err)
	}
	rt := moejs.NewRuntime(moejs.Options{})
	if err := rt.Load(mod); err != nil {
		t.Fatalf("Load: %v", err)
	}
	defer rt.ReleaseCallData()

	body, err := json.Marshal(planInput("manual"))
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	arg, err := rt.ParseJSONString(string(body))
	if err != nil {
		t.Fatalf("ParseJSONString: %v", err)
	}
	res, err := rt.Call(hook, arg)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var got struct {
		Action      string `json:"action"`
		PromptChars int    `json:"promptChars"`
		Request     struct {
			Messages []turn `json:"messages"`
		} `json:"request"`
	}
	if err := rt.Unmarshal(res, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Action != "takeover" {
		t.Fatalf("action = %q, want takeover", got.Action)
	}
	instr := instructionOf(t, got.Request.Messages)
	if got.PromptChars != len(instr) {
		t.Errorf("promptChars = %d, want the instruction length %d", got.PromptChars, len(instr))
	}
}

// TestParseSummarySkipsAnalysis: the analysis block goes first, whatever it
// quotes, even when it is repeated or truncated.
// validSummary is the smallest reply the completeness gate accepts: every one of
// the nine section names, each alone on its line. Fixtures wrap it in whatever
// shape they are about.
const validSummary = `1. Primary Request and Intent:
ship the compaction extension
2. Key Technical Concepts:
provider prompt cache
3. Files and Code Sections:
driver.go
4. Errors and fixes:
a rejected role
5. Problem Solving:
replaying the captured body
6. All user messages:
- go ahead
7. Pending Tasks:
none
8. Current Work:
tests
9. Optional Next Step:
run the gates`

func TestParseSummarySkipsAnalysis(t *testing.T) {
	p := mustPlugin(t)

	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "analysis quotes a fake summary",
			text: "<analysis>提到 <summary> 是假的</analysis><summary>" + validSummary + "</summary>",
			want: validSummary,
		},
		{
			name: "summary is trimmed",
			text: "<analysis>thoughts</analysis>\n<summary>\n  " + validSummary + "\n</summary>\n",
			want: validSummary,
		},
		{
			name: "no analysis block at all",
			text: "<summary>" + validSummary + "</summary>",
			want: validSummary,
		},
		{
			name: "repeated analysis blocks",
			text: "<analysis>a</analysis><analysis>b <summary>not this</summary></analysis>" +
				"<summary>" + validSummary + "</summary>",
			want: validSummary,
		},
		{
			// The measured failure: the model quotes the instruction it was given,
			// and that quotation carries a literal <summary> and </summary>. The
			// candidate that wins is the one carrying the nine sections, and
			// scoring candidates by their headings is what makes the quotation
			// lose, so a quoted </summary> no longer ends the block early.
			name: "summary quotes the instruction back",
			text: "<analysis>done</analysis><summary>CC_MARKER_42\n\n" +
				"the instruction said \u0060<summary>...</summary>\u0060, so:\n\n" +
				validSummary + "\n\nCC_MARKER_42 again</summary>",
			want: "CC_MARKER_42\n\n" +
				"the instruction said \u0060<summary>...</summary>\u0060, so:\n\n" +
				validSummary + "\n\nCC_MARKER_42 again",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := p.Parse(t.Context(), ParseInput{
				Text:         tc.text,
				StopReason:   "end_turn",
				OutputTokens: 900,
				Attempt:      0,
			})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if out.Summary != tc.want {
				t.Fatalf("summary = %q, want %q", out.Summary, tc.want)
			}
			if out.Retry || out.None {
				t.Errorf("retry = %v, none = %v, want both false", out.Retry, out.None)
			}
		})
	}
}

// TestParseSummaryRetryThenNone: no summary at all retries once, then hands the
// job back to PiG with the stop reason in the why.
func TestParseSummaryRetryThenNone(t *testing.T) {
	p := mustPlugin(t)

	first, err := p.Parse(t.Context(), ParseInput{
		Text:         "没有块",
		StopReason:   "end_turn",
		OutputTokens: 42,
		Attempt:      0,
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !first.Retry || first.None {
		t.Fatalf("retry = %v, none = %v, want retry", first.Retry, first.None)
	}
	if first.Summary != "" {
		t.Errorf("summary = %q, want empty", first.Summary)
	}
	if strings.TrimSpace(first.Why) == "" {
		t.Error("retry has an empty why")
	}

	second, err := p.Parse(t.Context(), ParseInput{
		Text:         "没有块",
		StopReason:   "end_turn",
		OutputTokens: 42,
		Attempt:      1,
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !second.None || second.Retry {
		t.Fatalf("retry = %v, none = %v, want none", second.Retry, second.None)
	}
	if !strings.Contains(second.Why, "end_turn") && !strings.Contains(second.Why, "42") {
		t.Errorf("why = %q, want the stop reason or the output tokens", second.Why)
	}
}

// TestParseSummaryEmptyBlock: an empty or truncated summary block is no summary,
// so it retries like a missing one.
func TestParseSummaryEmptyBlock(t *testing.T) {
	p := mustPlugin(t)

	cases := []struct {
		name string
		text string
	}{
		{"blank summary", "<analysis>x</analysis><summary>   \n\t </summary>"},
		{"only analysis", "<analysis>x <summary>fake</summary></analysis>"},
		{"truncated analysis", "<analysis>x <summary>fake</summary>"},
		{"empty text", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := p.Parse(t.Context(), ParseInput{
				Text:         tc.text,
				StopReason:   "max_tokens",
				OutputTokens: 7,
				Attempt:      0,
			})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if !out.Retry || out.None {
				t.Fatalf("summary = %q, retry = %v, none = %v, want retry", out.Summary, out.Retry, out.None)
			}
			if strings.TrimSpace(out.Why) == "" {
				t.Error("retry has an empty why")
			}
		})
	}
}

// TestParseSummaryGatesIncompleteReplies: a block that names too few of the nine
// sections is a reply that stopped early, and a length stop is untrustworthy even
// when the block is closed. Both retry once, then hand compaction back.
func TestParseSummaryGatesIncompleteReplies(t *testing.T) {
	p := mustPlugin(t)

	const twoSections = "1. Primary Request and Intent:\nship it\n2. Current Work:\ntests"
	const fiveSections = "1. Primary Request and Intent:\nship it\n2. Key Technical Concepts:\ncache\n" +
		"3. Files and Code Sections:\ndriver.go\n4. Errors and fixes:\nnone\n5. Problem Solving:\nreplay"

	cases := []struct {
		name     string
		text     string
		stop     string
		accepted bool
		wantWhy  string
	}{
		{
			name:    "two sections",
			text:    "<summary>" + twoSections + "</summary>",
			stop:    "stop",
			wantWhy: "of the nine sections",
		},
		{
			name:    "nine sections but a length stop",
			text:    "<summary>" + validSummary + "</summary>",
			stop:    "length",
			wantWhy: "output limit",
		},
		{
			name:     "five sections are enough",
			text:     "<summary>" + fiveSections + "</summary>",
			stop:     "stop",
			accepted: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parse := func(attempt int) ParseOutput {
				t.Helper()
				out, err := p.Parse(t.Context(), ParseInput{
					Text:         tc.text,
					StopReason:   tc.stop,
					OutputTokens: 900,
					Attempt:      attempt,
				})
				if err != nil {
					t.Fatalf("Parse(attempt %d): %v", attempt, err)
				}
				return out
			}
			first, second := parse(0), parse(1)

			if tc.accepted {
				if first.Summary == "" || second.Summary == "" {
					t.Fatalf("summary = %q / %q, want both attempts accepted", first.Summary, second.Summary)
				}
				return
			}
			if !first.Retry || first.None {
				t.Fatalf("attempt 0 = %+v, want a retry", first)
			}
			if !second.None || second.Retry {
				t.Fatalf("attempt 1 = %+v, want none", second)
			}
			if !strings.Contains(first.Why, tc.wantWhy) {
				t.Errorf("why = %q, want it to mention %q", first.Why, tc.wantWhy)
			}
		})
	}
}

// TestPlanConcurrent: eight goroutines share the pool and get the same answer.
func TestPlanConcurrent(t *testing.T) {
	p := mustPlugin(t)
	in := planInput("manual")

	const n = 8
	type result struct {
		out PlanOutput
		err error
	}
	// Results travel on a channel, so each goroutine touches only its own
	// variables and the shared plugin.
	results := make(chan result, n)
	ctx := t.Context()
	var wg sync.WaitGroup
	launch := func() {
		defer wg.Done()
		out, err := p.Plan(ctx, in)
		results <- result{out: out, err: err}
	}
	for range n {
		wg.Add(1)
		go launch()
	}
	wg.Wait()
	close(results)

	var outs []PlanOutput
	for res := range results {
		if res.err != nil {
			t.Fatalf("Plan: %v", res.err)
		}
		if res.out.Action != "takeover" {
			t.Fatalf("action = %q, want takeover", res.out.Action)
		}
		outs = append(outs, res.out)
	}
	if len(outs) != n {
		t.Fatalf("got %d results, want %d", len(outs), n)
	}
	for i := 1; i < len(outs); i++ {
		if canon(t, outs[i].Payload) != canon(t, outs[0].Payload) {
			t.Errorf("Plan %d payload differs from Plan 0:\n%s\n%s", i, outs[i].Payload, outs[0].Payload)
		}
		if canon(t, outs[i].Request) != canon(t, outs[0].Request) {
			t.Errorf("Plan %d request differs from Plan 0", i)
		}
	}
}

// TestPlanCanceledContext: an already cancelled context fails instead of
// running the policy.
func TestPlanCanceledContext(t *testing.T) {
	p := mustPlugin(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if out, err := p.Plan(ctx, planInput("manual")); err == nil {
		t.Fatalf("Plan on a cancelled context succeeded: %+v", out)
	}
	if out, err := p.Parse(ctx, ParseInput{Text: "没有块"}); err == nil {
		t.Fatalf("Parse on a cancelled context succeeded: %+v", out)
	}
}

// TestPlanInterruptsRunningJS: a context that expires while JavaScript runs
// stops it, and the runtime comes back to the pool usable.
func TestPlanInterruptsRunningJS(t *testing.T) {
	const src = `
let calls = 0;
export function planCompact(input) {
  calls += 1;
  if (calls === 1) { for (;;) {} }
  return { action: "pass", why: "already ran once" };
}
export function parseSummary(input) { return { summary: "x" }; }
`
	p := mustPluginFrom(t, src)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := p.Plan(ctx, planInput("manual"))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Plan succeeded although its context expired")
		}
		var interrupted *moejs.InterruptedError
		if !errors.As(err, &interrupted) {
			t.Fatalf("Plan error = %v, want an *moejs.InterruptedError", err)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Plan error does not wrap the context cause: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Plan hung on a running hook")
	}

	// The interrupted runtime went back to the pool cleared: the next call
	// takes the module's second branch, so it neither spins nor inherits the
	// interrupt.
	again, cancelAgain := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelAgain()
	out, err := p.Plan(again, planInput("manual"))
	if err != nil {
		t.Fatalf("Plan after an interrupt: %v", err)
	}
	if out.Action != "pass" || out.Why != "already ran once" {
		t.Fatalf("Plan after an interrupt = %+v, want the module's second call", out)
	}
}

// TestPlanJSErrorsAreErrors: a throw and a result that cannot be decoded are
// errors, never a panic.
func TestPlanJSErrorsAreErrors(t *testing.T) {
	throwing := mustPluginFrom(t, `
export function planCompact(input) { throw new Error("boom"); }
export function parseSummary(input) { throw new Error("parse boom"); }
`)
	out, err := throwing.Plan(t.Context(), planInput("manual"))
	if err == nil {
		t.Fatalf("Plan survived a throwing hook: %+v", out)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("Plan error = %v, want it to carry the thrown message", err)
	}
	if parsed, err := throwing.Parse(t.Context(), ParseInput{Text: "x"}); err == nil {
		t.Fatalf("Parse survived a throwing hook: %+v", parsed)
	}

	decoding := mustPluginFrom(t, `
export function planCompact(input) { return "not an object"; }
export function parseSummary(input) { return 42; }
`)
	if out, err := decoding.Plan(t.Context(), planInput("manual")); err == nil {
		t.Fatalf("Plan accepted a result it cannot decode: %+v", out)
	}
	if parsed, err := decoding.Parse(t.Context(), ParseInput{Text: "x"}); err == nil {
		t.Fatalf("Parse accepted a result it cannot decode: %+v", parsed)
	}

	// A hook that returns no value at all is an error too.
	empty := mustPluginFrom(t, `
export function planCompact(input) {}
export function parseSummary(input) {}
`)
	if out, err := empty.Plan(t.Context(), planInput("manual")); err == nil {
		t.Fatalf("Plan accepted an undefined result: %+v", out)
	}
}
