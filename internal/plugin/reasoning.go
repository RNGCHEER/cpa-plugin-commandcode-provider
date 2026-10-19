package plugin

import (
	"bytes"
	"encoding/json"

	"github.com/tidwall/sjson"
)

// CommandCode's chat-completions upstream reports reasoning in its own fields
// (reasoning and reasoning_details[].text) and never uses the standard
// reasoning_content key. Both the host's openai->claude translator and
// downstream OpenAI clients read reasoning_content, so every response and
// every stream chunk is backfilled before it leaves the plugin.

// mapReasoningBody backfills reasoning_content when the upstream reported
// reasoning text under its own keys. Returns (body, true) when a rewrite
// happened, (body, false) when the body was already standard or unreadable.
func mapReasoningBody(body []byte) ([]byte, bool) {
	if len(body) == 0 || !json.Valid(body) {
		return body, false
	}
	var probe struct {
		Reasoning        string          `json:"reasoning"`
		ReasoningContent string          `json:"reasoning_content"`
		ReasoningDetails json.RawMessage `json:"reasoning_details"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return body, false
	}
	if probe.ReasoningContent != "" || probe.Reasoning == "" {
		if probe.ReasoningContent == "" && len(probe.ReasoningDetails) > 0 {
			if text := firstReasoningDetail(probe.ReasoningDetails); text != "" {
				out, err := sjson.SetBytes(body, "reasoning_content", text)
				if err != nil {
					return body, false
				}
				return out, true
			}
		}
		return body, false
	}
	out, err := sjson.SetBytes(body, "reasoning_content", probe.Reasoning)
	if err != nil {
		return body, false
	}
	return out, true
}

// firstReasoningDetail extracts the first non-empty text field from the
// upstream's reasoning_details array, whichever entry shape it uses.
func firstReasoningDetail(raw json.RawMessage) string {
	var entries []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		var text string
		if err := json.Unmarshal(raw, &text); err == nil {
			return text
		}
		return ""
	}
	for _, entry := range entries {
		if entry.Text != "" {
			return entry.Text
		}
	}
	return ""
}

// streamLine is one complete upstream SSE line with its "data: " framing
// already stripped (bare JSON payload).
type streamLine struct {
	payload []byte
	isData  bool
}

// parseStreamLines splits raw host-stream bytes into complete SSE lines. The
// host delivers arbitrary reads, so a line can straddle chunks; the caller
// keeps the returned remainder for the next read.
func parseStreamLines(buf []byte) (lines []streamLine, rest []byte) {
	for {
		idx := bytes.IndexByte(buf, '\n')
		if idx < 0 {
			break
		}
		raw := buf[:idx+1]
		buf = buf[idx+1:]
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 {
			// Blank lines are SSE event separators; drop them (framing is
			// re-added per route when emitting).
			continue
		}
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			payload := bytes.TrimSpace(trimmed[len("data:"):])
			for bytes.HasPrefix(payload, []byte("data:")) {
				payload = bytes.TrimSpace(payload[len("data:"):])
			}
			if len(payload) == 0 || string(payload) == "[DONE]" || !json.Valid(payload) {
				continue
			}
			lines = append(lines, streamLine{payload: payload, isData: true})
			continue
		}
		if bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte(":")) {
			continue
		}
		// Non-SSE bytes: never forward raw, they would corrupt the stream.
	}
	return lines, buf
}

// backfillChunk rewrites one upstream SSE data payload in place (reasoning
// backfill) so every route sees standard reasoning_content.
func backfillChunk(payload []byte) []byte {
	fixed, ok := mapReasoningBody(payload)
	if !ok {
		return payload
	}
	return fixed
}

// stripClientFraming removes a "data: " prefix some client-facing routes
// already carry in the payload they handed us; the upstream transport frames
// lines itself, so a doubled prefix would break SSE parsing upstream.
func stripClientFraming(payload []byte) []byte {
	trimmed := bytes.TrimSpace(payload)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return bytes.TrimSpace(trimmed[len("data:"):])
	}
	return payload
}
