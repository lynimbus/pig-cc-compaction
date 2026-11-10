package pigcc

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

// turnPrefixInstruction is Pi's turnPrefixSummarizationPrompt
// (internal/codingagent/compaction/compaction.go), verbatim.
//
// It lives here rather than in plugin/compaction.js because it cannot be sent
// on its own: Pi prefixes it with a conversation serialized by the SDK
// (SerializeConversation over convertToLlm), and the assembled text is what the
// capture guard below has to recognize. The Claude Code prompt next door needs
// none of that, so it stays in JS where it can be edited in place.
const turnPrefixInstruction = `The messages above are earlier context from an ongoing conversation. Later messages are stored separately and do not need to be reconstructed.

Create a concise checkpoint of the user's request and the progress shown above. This checkpoint will be placed before the later messages so the conversation can continue with the necessary context.

## Original Request
[What did the user ask for?]

## Progress So Far
- [Key decisions and work completed in these messages]

## Context Needed to Continue
- [Information from these messages needed to understand the later work]

Only summarize information explicitly present above. Do not infer or recreate later messages.`

// turnPrefixMarker is a stable sentence of turnPrefixInstruction, used to
// recognize this extension's own prefix request so that capturing it does not
// overwrite the session prefix it was built from.
const turnPrefixMarker = "Create a concise checkpoint of the user's request and the progress shown above."

// splitTurnSeparator is Pi's own punctuation between the history summary and the
// split-turn checkpoint (compaction.go: "historySummary + \"\n\n---\n\n**Turn ...").
const splitTurnSeparator = "\n\n---\n\n**Turn Context (split turn):**\n\n"

// summarizeTurnPrefixPrompt builds the request text of a split-turn prefix
// summary, byte for byte as Pi builds it.
func summarizeTurnPrefixPrompt(prefix []json.RawMessage, customInstructions string) (string, error) {
	conversation, err := serializeTurnPrefix(prefix)
	if err != nil {
		return "", err
	}
	prompt := "# Conversation\n" + conversation + "\n\n# Instructions\n" + turnPrefixInstruction
	if custom := strings.TrimSpace(customInstructions); custom != "" {
		prompt += "\n\nAdditional focus: " + custom
	}
	return prompt, nil
}

// serializeTurnPrefix renders agent messages the way Pi does before summarizing
// them: convertToLlm, then Pi's own serializer, both from the shipped SDK so the
// text is the host's and not a reimplementation.
func serializeTurnPrefix(prefix []json.RawMessage) (string, error) {
	agents := make([]map[string]any, 0, len(prefix))
	for _, raw := range prefix {
		message := make(map[string]any)
		if err := json.Unmarshal(raw, &message); err != nil {
			return "", fmt.Errorf("decode a turn prefix message: %w", err)
		}
		agents = append(agents, message)
	}
	llm := sdk.ConvertToLlm(agents)
	raw := make([]json.RawMessage, 0, len(llm))
	for _, message := range llm {
		encoded, err := json.Marshal(message)
		if err != nil {
			return "", fmt.Errorf("encode a turn prefix message: %w", err)
		}
		raw = append(raw, encoded)
	}
	serialized, err := sdk.SerializeConversation(raw)
	if err != nil {
		return "", fmt.Errorf("serialize the turn prefix: %w", err)
	}
	return serialized, nil
}

// fitTurnPrefix keeps only the tail of a split turn's prefix when the whole of
// it would not fit the summary request's window.
//
// Pi splits the prefix in two there and pushes the head into the history
// summary (a documented divergence, D167). This extension does not have to: its
// main request carries the entire conversation already, so the head is in the
// nine sections regardless, and only the checkpoint loses detail.
func fitTurnPrefix(prefix []json.RawMessage, window, maxOutput int) []json.RawMessage {
	if window <= 0 || len(prefix) < 2 {
		return prefix
	}
	// Pi's own budget: the instructions plus two output limits come off the
	// window, and the conversation may fill three quarters of what is left.
	room := window - 2*maxOutput - utf8.RuneCountInString(turnPrefixInstruction)/4
	if room <= 0 {
		return prefix
	}
	room = room * 3 / 4

	sizes := make([]int, len(prefix))
	total := 0
	for index, raw := range prefix {
		sizes[index] = estimateMessageTokens(raw)
		total += sizes[index]
	}
	if total <= room {
		return prefix
	}

	used, start := 0, len(prefix)
	for index := range slices.Backward(sizes) {
		if used > 0 && used+sizes[index] > room {
			break
		}
		used += sizes[index]
		start = index
	}
	return prefix[start:]
}

// estimateMessageTokens counts one agent message the way the SDK does.
func estimateMessageTokens(raw json.RawMessage) int {
	message := make(map[string]any)
	if err := json.Unmarshal(raw, &message); err != nil {
		return 0
	}
	if tokens := sdk.EstimateTokens(message); tokens > 0 {
		return tokens
	}
	return utf8.RuneCount(raw)/4 + 1
}

// turnPrefixPayload is the provider body of a split-turn prefix request: the
// captured body keeps its system prompt and sampling parameters, so the request
// still shares the longest possible prefix with the one it summarizes, while
// messages carry nothing but the prompt and the session's tools stay out of it.
func turnPrefixPayload(captured json.RawMessage, prompt string) (json.RawMessage, error) {
	body := make(map[string]any)
	if err := json.Unmarshal(captured, &body); err != nil {
		return nil, fmt.Errorf("decode the captured body: %w", err)
	}
	body["messages"] = []any{map[string]any{
		"role":    "user",
		"content": []any{map[string]any{"type": "text", "text": prompt}},
	}}
	for _, key := range []string{"tools", "tool_choice", "functions", "function_call", "parallel_tool_calls"} {
		delete(body, key)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode the turn prefix body: %w", err)
	}
	return encoded, nil
}

// turnPrefixRequest is the host-level request of the same call. It carries the
// prompt alone: the payload above replaces the body before it reaches the
// provider.
func turnPrefixRequest(prompt string) json.RawMessage {
	request, err := json.Marshal(map[string]any{
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": prompt}},
		}},
	})
	if err != nil {
		return nil
	}
	return request
}

// contextWindowOf and maxOutputOf read the model map the host resolves, which
// carries the provider's capabilities when it knows them.
func contextWindowOf(model map[string]any) int {
	return capability(model, "contextWindow")
}

func maxOutputOf(model map[string]any) int {
	return capability(model, "maxOutputTokens")
}

func capability(model map[string]any, name string) int {
	if model == nil {
		return 0
	}
	capabilities, ok := model["capabilities"].(map[string]any)
	if !ok {
		return 0
	}
	return intOf(capabilities[name])
}

// cleanStop reports whether a prefix reply ended on its own. The checkpoint is
// auxiliary, so only a clean stop is kept: a reply cut at the output limit would
// put an unfinished paragraph in front of the retained turn.
func cleanStop(stop string) bool {
	switch stop {
	case "", "end_turn", "stop", "stop_sequence":
		return true
	}
	return false
}
