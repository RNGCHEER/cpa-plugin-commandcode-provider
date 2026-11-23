package plugin

import "encoding/json"

// Chat Completions -> standard Responses, without Codex-specific defaults.
func chatToResponses(chat []byte, id string, stream bool) ([]byte, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(chat, &req); err != nil {
		return nil, err
	}
	var messages []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []chatToolCall  `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
	}
	if err := json.Unmarshal(req["messages"], &messages); err != nil {
		return nil, err
	}
	input := []any{}
	for _, msg := range messages {
		if msg.Role == "tool" {
			input = append(input, map[string]any{"type": "function_call_output", "call_id": msg.ToolCallID, "output": stringFromJSON(msg.Content)})
			continue
		}
		var text string
		var content any
		if json.Unmarshal(msg.Content, &text) == nil {
			content = text
		} else {
			var parts []map[string]any
			_ = json.Unmarshal(msg.Content, &parts)
			out := []any{}
			for _, part := range parts {
				switch part["type"] {
				case "text":
					kind := "input_text"
					if msg.Role == "assistant" {
						kind = "output_text"
					}
					out = append(out, map[string]any{"type": kind, "text": part["text"]})
				case "image_url":
					if image, ok := part["image_url"].(map[string]any); ok {
						item := map[string]any{"type": "input_image", "image_url": image["url"]}
						if image["detail"] != nil {
							item["detail"] = image["detail"]
						}
						out = append(out, item)
					}
				}
			}
			content = out
		}
		if len(msg.Content) > 0 && string(msg.Content) != "null" {
			input = append(input, map[string]any{"type": "message", "role": msg.Role, "content": content})
		}
		for _, call := range msg.ToolCalls {
			input = append(input, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
		}
	}
	out := map[string]any{"model": id, "stream": stream, "input": input}
	for _, field := range []string{"temperature", "top_p", "parallel_tool_calls", "store", "metadata", "service_tier", "reasoning"} {
		if raw, ok := req[field]; ok {
			out[field] = raw
		}
	}
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		if raw, ok := req[field]; ok {
			out["max_output_tokens"] = raw
		}
	}
	if raw, ok := req["reasoning_effort"]; ok {
		out["reasoning"] = map[string]any{"effort": raw}
	}
	var tools []map[string]any
	if json.Unmarshal(req["tools"], &tools) == nil {
		converted := []any{}
		for _, tool := range tools {
			if fn, ok := tool["function"].(map[string]any); ok {
				fn["type"] = "function"
				converted = append(converted, fn)
			}
		}
		out["tools"] = converted
	}
	if raw, ok := req["tool_choice"]; ok {
		var choice map[string]any
		if json.Unmarshal(raw, &choice) == nil {
			if fn, ok := choice["function"].(map[string]any); ok {
				out["tool_choice"] = map[string]any{"type": "function", "name": fn["name"]}
			}
		} else {
			out["tool_choice"] = raw
		}
	}
	if raw, ok := req["response_format"]; ok {
		out["text"] = map[string]any{"format": raw}
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return repairResponses(body, id), nil
}
