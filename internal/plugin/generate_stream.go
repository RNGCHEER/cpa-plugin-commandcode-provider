package plugin

// Response side of the CLI gateway port: /alpha/generate streams
// newline-delimited JSON events ({"type":"text-delta",...}), while the host's
// executor contract and every downstream client expect OpenAI
// chat-completions. This file bridges the two, mirroring the upstream ports'
// event handling (patlux pi-commandcode-provider src/core.ts, Mars-Sea
// dsh-commandcode-provider stream handling).

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// cliToolCall accumulates one gateway tool call across its start/delta/end
// events.
type cliToolCall struct {
	id        string
	name      string
	args      strings.Builder
	announced bool
}

// cliTranslator turns CLI gateway events into OpenAI chat-completions. In
// streaming mode every event yields zero or more `chat.completion.chunk`
// payloads; in aggregate mode the whole stream collapses into one
// `chat.completion` body.
type cliTranslator struct {
	model   string
	id      string
	created int64
	stream  bool

	roleSent bool

	content   strings.Builder
	reasoning strings.Builder
	tools     []*cliToolCall

	finishReason string
	usage        *cliUsage
	terminalSent bool
}

func newCLITranslator(model string, stream bool) *cliTranslator {
	return &cliTranslator{
		model:   model,
		id:      newChatCompletionID(),
		created: time.Now().Unix(),
		stream:  stream,
	}
}

func newChatCompletionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// feed consumes one gateway event line. It returns the OpenAI chunk payloads
// that line produced (empty for events that carry no client-visible content)
// and an error for a terminal gateway error event.
func (t *cliTranslator) feed(line []byte) ([][]byte, error) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) == 0 || string(trimmed) == "[DONE]" {
		return nil, nil
	}
	var ev cliEvent
	if err := json.Unmarshal(trimmed, &ev); err != nil {
		// Non-JSON noise on the wire is never forwarded downstream.
		return nil, nil
	}

	switch ev.Type {
	case "reasoning-delta":
		t.reasoning.WriteString(ev.Text)
		if t.stream && ev.Text != "" {
			return t.delta(map[string]any{"reasoning_content": ev.Text}), nil
		}

	case "text-delta":
		t.content.WriteString(ev.Text)
		if t.stream && ev.Text != "" {
			return t.delta(map[string]any{"content": ev.Text}), nil
		}

	case "tool-input-start":
		tc := t.ensureTool(ev.ID, ev.ToolName)
		if t.stream {
			return [][]byte{t.toolStartChunk(tc)}, nil
		}

	case "tool-input-delta":
		tc := t.ensureTool(ev.ID, "")
		if ev.Delta != "" {
			tc.args.WriteString(ev.Delta)
		}
		if t.stream && ev.Delta != "" {
			return t.delta(t.toolArgsDelta(tc, ev.Delta)), nil
		}

	case "tool-call":
		tc := t.ensureTool(ev.ToolCallID, ev.ToolName)
		if tc.name == "" {
			tc.name = ev.ToolName
		}
		if tc.args.Len() == 0 {
			args := compactInput(ev.Input)
			tc.args.WriteString(args)
			if t.stream {
				out := make([][]byte, 0, 2)
				if !tc.announced {
					out = append(out, t.toolStartChunk(tc))
				}
				out = append(out, t.delta(t.toolArgsDelta(tc, args))...)
				return out, nil
			}
		}

	case "finish", "finish-step":
		if ev.TotalUsage != nil {
			t.usage = ev.TotalUsage
		} else if ev.Usage != nil && t.usage == nil {
			t.usage = ev.Usage
		}
		if reason := firstNonBlank(ev.FinishReason, ev.RawFinishReason); reason != "" {
			// "finish" carries the request-level reason; "finish-step" is the
			// per-step one, so the request-level value wins.
			if ev.Type == "finish" || t.finishReason == "" {
				t.finishReason = reason
			}
		}
		if t.stream && ev.Type == "finish" {
			return t.terminal(), nil
		}

	case "error":
		return nil, fmt.Errorf("commandcode: %s", cliErrorMessage(ev))
	}
	return nil, nil
}

// terminal emits the final chunk (finish_reason + usage) exactly once.
func (t *cliTranslator) terminal() [][]byte {
	if t.terminalSent {
		return nil
	}
	t.terminalSent = true
	choice := map[string]any{
		"index":         0,
		"delta":         map[string]any{},
		"finish_reason": cliFinishReason(t.finishReason),
	}
	chunk := t.chunkBase()
	chunk["choices"] = []any{choice}
	if t.usage != nil {
		chunk["usage"] = t.usage.openAI()
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	return [][]byte{raw}
}

// finish flushes the terminal chunk for streams that ended without a "finish"
// event (for example when the connection drops after the last content delta).
func (t *cliTranslator) finish() [][]byte {
	if !t.stream {
		return nil
	}
	return t.terminal()
}

func (t *cliTranslator) chunkBase() map[string]any {
	return map[string]any{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"created": t.created,
		"model":   t.model,
	}
}

func (t *cliTranslator) delta(delta map[string]any) [][]byte {
	if !t.roleSent {
		t.roleSent = true
		delta["role"] = "assistant"
	}
	chunk := t.chunkBase()
	chunk["choices"] = []any{map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	return [][]byte{raw}
}

func (t *cliTranslator) toolStartChunk(tc *cliToolCall) []byte {
	tc.announced = true
	chunk := t.chunkBase()
	delta := map[string]any{
		"tool_calls": []any{map[string]any{
			"index": t.toolIndex(tc),
			"id":    tc.id,
			"type":  "function",
			"function": map[string]any{
				"name":      tc.name,
				"arguments": "",
			},
		}},
	}
	if !t.roleSent {
		t.roleSent = true
		delta["role"] = "assistant"
	}
	chunk["choices"] = []any{map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
	}}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	return raw
}

func (t *cliTranslator) ensureTool(id, name string) *cliToolCall {
	if id == "" {
		id = fmt.Sprintf("call_%d", len(t.tools))
	}
	for _, tc := range t.tools {
		if tc.id == id {
			if tc.name == "" && name != "" {
				tc.name = name
			}
			return tc
		}
	}
	tc := &cliToolCall{id: id, name: name}
	t.tools = append(t.tools, tc)
	return tc
}

func (t *cliTranslator) toolIndex(tc *cliToolCall) int {
	for i, candidate := range t.tools {
		if candidate == tc {
			return i
		}
	}
	return 0
}

// completion renders the aggregated non-streaming answer.
func (t *cliTranslator) completion() []byte {
	message := map[string]any{"role": "assistant"}
	if calls := t.toolCallsJSON(); len(calls) > 0 {
		message["tool_calls"] = calls
		if t.content.Len() > 0 {
			message["content"] = t.content.String()
		} else {
			message["content"] = nil
		}
	} else {
		message["content"] = t.content.String()
	}
	if t.reasoning.Len() > 0 {
		message["reasoning_content"] = t.reasoning.String()
	}
	out := map[string]any{
		"id":      t.id,
		"object":  "chat.completion",
		"created": t.created,
		"model":   t.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": cliFinishReason(t.finishReason),
		}},
	}
	if t.usage != nil {
		out["usage"] = t.usage.openAI()
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return []byte(`{"error":{"message":"failed to encode response"}}`)
	}
	return raw
}

func (t *cliTranslator) toolCallsJSON() []any {
	if len(t.tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(t.tools))
	for _, tc := range t.tools {
		args := tc.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out, map[string]any{
			"id":   tc.id,
			"type": "function",
			"function": map[string]any{
				"name":      tc.name,
				"arguments": args,
			},
		})
	}
	return out
}

// toolArgsDelta renders one tool-call arguments fragment for the tool's
// position in the message.
func (t *cliTranslator) toolArgsDelta(tc *cliToolCall, delta string) map[string]any {
	return map[string]any{
		"tool_calls": []any{map[string]any{
			"index": t.toolIndex(tc),
			"function": map[string]any{
				"arguments": delta,
			},
		}},
	}
}

// openAI renders the gateway usage in OpenAI shape.
func (u *cliUsage) openAI() map[string]any {
	prompt := u.InputTokens
	completion := u.OutputTokens
	total := u.TotalTokens
	if total == 0 {
		total = prompt + completion
	}
	out := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}
	if u.InputTokenDetails.CacheReadTokens > 0 {
		out["prompt_tokens_details"] = map[string]any{
			"cached_tokens": u.InputTokenDetails.CacheReadTokens,
		}
	}
	return out
}

// cliFinishReason maps the gateway's finish reasons onto OpenAI's.
func cliFinishReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "tool-calls", "tool_calls", "tooluse", "tool-use":
		return "tool_calls"
	case "length", "max_tokens", "max-tokens", "max_output_tokens", "max-output-tokens":
		return "length"
	case "content-filter", "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}

// compactInput renders a gateway tool-call input object as a JSON argument
// string.
func compactInput(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "{}"
	}
	if buf.Len() == 0 {
		return "{}"
	}
	return buf.String()
}

// cliErrorMessage extracts a readable message from a gateway error event.
func cliErrorMessage(ev cliEvent) string {
	if trimmed := strings.TrimSpace(ev.Message); trimmed != "" {
		return trimmed
	}
	if len(ev.Error) > 0 && string(ev.Error) != "null" {
		if msg := errorMessage(ev.Error); msg != "" {
			return msg
		}
		return strings.TrimSpace(string(ev.Error))
	}
	return "gateway reported an error"
}

// aggregateGenerateResponse collapses a complete (buffered) gateway stream into
// one non-streaming chat-completion body.
func aggregateGenerateResponse(body []byte, model string) ([]byte, error) {
	tr := newCLITranslator(model, false)
	for _, line := range splitNDJSON(body) {
		if _, err := tr.feed(line); err != nil {
			return nil, err
		}
	}
	if len(tr.tools) == 0 && tr.content.Len() == 0 && tr.reasoning.Len() == 0 && tr.finishReason == "" {
		return nil, fmt.Errorf("commandcode: gateway returned no events")
	}
	return tr.completion(), nil
}

// splitNDJSON splits a buffered newline-delimited JSON stream into lines.
func splitNDJSON(body []byte) [][]byte {
	lines := bytes.Split(body, []byte{'\n'})
	out := make([][]byte, 0, len(lines))
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) > 0 {
			out = append(out, line)
		}
	}
	return out
}
