package pigcc

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSummarizeTurnPrefixPromptKeepsPisShape: the request is Pi's own, so a
// change in the wrapper shows up here rather than in a diff of the summary.
func TestSummarizeTurnPrefixPromptKeepsPisShape(t *testing.T) {
	prefix := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"fix the flaky test"}]}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"looking at the runner"}]}`),
	}

	got, err := summarizeTurnPrefixPrompt(prefix, "")
	if err != nil {
		t.Fatalf("summarizeTurnPrefixPrompt: %v", err)
	}
	want := "# Conversation\n[User]: fix the flaky test\n\n[Assistant]: looking at the runner\n\n# Instructions\n" + turnPrefixInstruction
	if got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}

func TestSummarizeTurnPrefixPromptCarriesTheFocus(t *testing.T) {
	prefix := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hello"}]}`),
	}

	got, err := summarizeTurnPrefixPrompt(prefix, "  keep the migration decisions  ")
	if err != nil {
		t.Fatalf("summarizeTurnPrefixPrompt: %v", err)
	}
	if !strings.HasSuffix(got, "Additional focus: keep the migration decisions") {
		t.Fatalf("prompt = %q, want the focus appended", got)
	}
}

func TestSummarizeTurnPrefixPromptRejectsUnreadableMessages(t *testing.T) {
	prefix := []json.RawMessage{json.RawMessage(`{"role":"assistant","content":"not a block list"}`)}
	if _, err := summarizeTurnPrefixPrompt(prefix, ""); err == nil {
		t.Fatal("summarizeTurnPrefixPrompt accepted a message the serializer cannot read")
	}
}

// TestFitTurnPrefixKeepsTheTail: the main request already carries the whole
// conversation, so an oversized prefix loses its head instead of failing.
func TestFitTurnPrefixKeepsTheTail(t *testing.T) {
	big := strings.Repeat("x", 4000)
	prefix := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"` + big + `"}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"` + big + `"}]}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"text","text":"the last one"}]}`),
	}

	if got := fitTurnPrefix(prefix, 0, 0); len(got) != len(prefix) {
		t.Fatalf("fitTurnPrefix without a window kept %d messages, want all %d", len(got), len(prefix))
	}
	if got := fitTurnPrefix(prefix, 200000, 4096); len(got) != len(prefix) {
		t.Fatalf("fitTurnPrefix with room kept %d messages, want all %d", len(got), len(prefix))
	}

	got := fitTurnPrefix(prefix, 9000, 4096)
	if len(got) == 0 || len(got) >= len(prefix) {
		t.Fatalf("fitTurnPrefix kept %d of %d messages, want a shorter tail", len(got), len(prefix))
	}
	if !strings.Contains(string(got[len(got)-1]), "the last one") {
		t.Fatalf("fitTurnPrefix dropped the newest message: %s", got[len(got)-1])
	}
}

// TestTurnPrefixPayloadReplacesMessagesAndDropsTools: the body starts from the
// captured one, so the system prompt stays the prefix the provider cached.
func TestTurnPrefixPayloadReplacesMessagesAndDropsTools(t *testing.T) {
	captured := json.RawMessage(`{"model":"deepseek","temperature":0.2,"system":"you are helpful","tools":[{"name":"read"}],"tool_choice":"auto","messages":[{"role":"user","content":"hi"}]}`)

	raw, err := turnPrefixPayload(captured, "# Conversation\n[User]: hi")
	if err != nil {
		t.Fatalf("turnPrefixPayload: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if body["system"] != "you are helpful" || body["model"] != "deepseek" {
		t.Fatalf("payload lost the captured prefix: %v", body)
	}
	for _, key := range []string{"tools", "tool_choice"} {
		if _, ok := body[key]; ok {
			t.Fatalf("payload kept %q, want the tools dropped from a summary request", key)
		}
	}
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("messages = %v, want exactly the prompt", body["messages"])
	}
	if !strings.Contains(string(raw), "# Conversation") {
		t.Fatalf("payload = %s, want it to carry the prompt", raw)
	}
}

func TestTurnPrefixPayloadRejectsANonObjectBody(t *testing.T) {
	if _, err := turnPrefixPayload(json.RawMessage(`[1,2]`), "prompt"); err == nil {
		t.Fatal("turnPrefixPayload accepted a body that is not an object")
	}
}

// TestTurnPrefixRequestIsCapturedAsOurs: every request this driver sends has to
// be recognizable to the capture guard, or the next compaction resends the
// prefix summary instead of the session.
func TestTurnPrefixRequestIsCapturedAsOurs(t *testing.T) {
	prefix := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"text","text":"hello"}]}`),
	}
	prompt, err := summarizeTurnPrefixPrompt(prefix, "")
	if err != nil {
		t.Fatalf("summarizeTurnPrefixPrompt: %v", err)
	}

	request := turnPrefixRequest(prompt)
	if len(request) == 0 {
		t.Fatal("turnPrefixRequest produced no request")
	}
	if !isSummarizationRequest(request) {
		t.Fatalf("the capture guard does not recognize the prefix request: %s", request)
	}
	// And the guard still recognizes the main request.
	if !isSummarizationText(summarizationMarker) {
		t.Fatal("the capture guard does not recognize the main summarization marker")
	}
}

func TestCleanStop(t *testing.T) {
	for _, stop := range []string{"", "end_turn", "stop", "stop_sequence"} {
		if !cleanStop(stop) {
			t.Errorf("cleanStop(%q) = false, want true", stop)
		}
	}
	for _, stop := range []string{"length", "max_tokens", "tool_use", "content_filter"} {
		if cleanStop(stop) {
			t.Errorf("cleanStop(%q) = true, want false", stop)
		}
	}
}
