package plugin

import (
	"encoding/json"
	"fmt"

	"github.com/tidwall/sjson"
)

// TranslateRequest rewrites one client payload (openai / claude /
// openai-response) into the single upstream transport this port speaks:
// OpenAI chat-completions against POST {base}/chat/completions.
//
// The rule is deliberately mechanical: take the client body as the payload,
// force the upstream model id, force the stream flag, and drop transport-only
// fields the upstream rejects (stop sequences are refused by CommandCode).
// Everything else - messages, tools, tool_choice, reasoning, images - passes
// through because the upstream IS an OpenAI chat-completions surface.
//
// claude / openai-response bodies are converted message-by-message: they are
// already structured chat turns, only their envelope differs.
func TranslateRequest(sourceFormat string, original []byte, catalogID string, stream bool) ([]byte, error) {
	if len(original) == 0 {
		return nil, fmt.Errorf("empty request body")
	}
	var payload []byte
	var err error
	switch sourceFormat {
	case "", "openai", "chat-completions":
		payload = append([]byte(nil), original...)
	case "claude":
		payload, err = claudeToChat(original, catalogID, stream)
		if err != nil {
			return nil, err
		}
	case "openai-response", "responses":
		payload, err = responsesToChat(original, catalogID, stream)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported source format %q", sourceFormat)
	}
	if !json.Valid(payload) {
		return nil, fmt.Errorf("request body is not valid JSON")
	}
	// The upstream model id is authoritative: the client-visible prefix (or a
	// host alias) never reaches the wire.
	if payload, err = sjson.SetBytes(payload, "model", catalogID); err != nil {
		return nil, fmt.Errorf("set model: %w", err)
	}
	if payload, err = sjson.SetBytes(payload, "stream", stream); err != nil {
		return nil, fmt.Errorf("set stream: %w", err)
	}
	if stream {
		// Usage in the final chunk lets CPA's usage accounting see real
		// token counts on every stream.
		if payload, err = sjson.SetBytes(payload, "stream_options.include_usage", true); err != nil {
			return nil, fmt.Errorf("set stream_options: %w", err)
		}
	}
	// CommandCode refuses requests carrying stop sequences; drop them rather
	// than fail the whole turn.
	if payload, err = sjson.DeleteBytes(payload, "stop"); err != nil {
		return nil, fmt.Errorf("drop stop: %w", err)
	}
	return payload, nil
}

// claudeToChat converts an Anthropic Messages body into a chat-completions
// body: system -> system, messages -> messages (content blocks flattened),
// tools -> functions, max_tokens kept (it is a valid chat field), stop
// sequences dropped upstream by the shared tail.
func claudeToChat(original []byte, catalogID string, stream bool) ([]byte, error) {
	var req struct {
		System    json.RawMessage `json:"system"`
		Messages  []claudeMessage `json:"messages"`
		Tools     []claudeTool    `json:"tools"`
		MaxTokens int             `json:"max_tokens"`
	}
	if err := json.Unmarshal(original, &req); err != nil {
		return nil, fmt.Errorf("decode claude request: %w", err)
	}
	out, _ := sjson.SetBytes([]byte("{}"), "model", catalogID)
	out, _ = sjson.SetBytes(out, "stream", stream)
	if len(req.System) > 0 && string(req.System) != "null" {
		text, err := claudeText(req.System)
		if err != nil {
			return nil, err
		}
		if text != "" {
			out, _ = sjson.SetBytes(out, "system", text)
		}
	}
	messages := make([]map[string]any, 0, len(req.Messages))
	for _, msg := range req.Messages {
		content, err := claudeContentToChat(msg.Content)
		if err != nil {
			return nil, err
		}
		messages = append(messages, map[string]any{"role": msg.Role, "content": content})
	}
	if len(messages) > 0 {
		out, _ = sjson.SetBytes(out, "messages", messages)
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			schema := tool.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{
				"type":      "function",
				"name":      tool.Name,
				"arguments": schema,
			})
		}
		out, _ = sjson.SetBytes(out, "tools", tools)
	}
	if req.MaxTokens > 0 {
		out, _ = sjson.SetBytes(out, "max_tokens", req.MaxTokens)
	}
	return out, nil
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type claudeTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// claudeContentToChat flattens a Claude content value (string or block array)
// into the OpenAI message content shape: plain text stays a string, image
// blocks become image_url parts.
func claudeContentToChat(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var blocks []struct {
		Type   string `json:"type"`
		Text   string `json:"text"`
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("decode claude content: %w", err)
	}
	parts := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": block.Text})
		case "image":
			url := block.Source.URL
			if url == "" && block.Source.Data != "" {
				media := block.Source.MediaType
				if media == "" {
					media = "image/png"
				}
				url = "data:" + media + ";base64," + block.Source.Data
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		default:
			// tool_result / thinking / other blocks are not chat content;
			// skip them (the host's own translators replay paired history).
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	return parts, nil
}

// claudeText renders a Claude system value (string or text blocks) as one
// string for the chat system field.
func claudeText(raw json.RawMessage) (string, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("decode claude system: %w", err)
	}
	out := ""
	for i, block := range blocks {
		if block.Type != "text" && block.Type != "" {
			continue
		}
		if i > 0 && out != "" {
			out += "\n\n"
		}
		out += block.Text
	}
	return out, nil
}

// responsesToChat converts an OpenAI Responses body into chat-completions:
// input items become chat messages, instructions become the system prompt,
// and tools become functions.
func responsesToChat(original []byte, catalogID string, stream bool) ([]byte, error) {
	var req struct {
		Instructions string          `json:"instructions"`
		Input        json.RawMessage `json:"input"`
		Tools        []responsesTool `json:"tools"`
		MaxOutput    int             `json:"max_output_tokens"`
	}
	if err := json.Unmarshal(original, &req); err != nil {
		return nil, fmt.Errorf("decode responses request: %w", err)
	}
	out, _ := sjson.SetBytes([]byte("{}"), "model", catalogID)
	out, _ = sjson.SetBytes(out, "stream", stream)
	if instr := strings_Trim(req.Instructions); instr != "" {
		out, _ = sjson.SetBytes(out, "system", instr)
	}
	items, err := responsesItems(req.Input)
	if err != nil {
		return nil, err
	}
	messages := make([]map[string]any, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case "message", "":
			content, err := responsesContentToChat(item.Content)
			if err != nil {
				return nil, err
			}
			role := item.Role
			if role == "" {
				role = "user"
			}
			messages = append(messages, map[string]any{"role": role, "content": content})
		case "function_call":
			messages = append(messages, map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []map[string]any{{
					"id":   item.CallID,
					"type": "function",
					"function": map[string]any{
						"name":      item.Name,
						"arguments": item.Arguments,
					},
				}},
			})
		case "function_call_output":
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": item.CallID,
				"content":      item.Output,
			})
		default:
			// reasoning / other item types are not chat content; skip.
		}
	}
	if len(messages) > 0 {
		out, _ = sjson.SetBytes(out, "messages", messages)
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			schema := tool.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{
				"type":      "function",
				"name":      tool.Name,
				"arguments": schema,
			})
		}
		out, _ = sjson.SetBytes(out, "tools", tools)
	}
	if req.MaxOutput > 0 {
		out, _ = sjson.SetBytes(out, "max_tokens", req.MaxOutput)
	}
	return out, nil
}

type responsesTool struct {
	Name       string          `json:"name"`
	Parameters json.RawMessage `json:"parameters"`
}

type responsesItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// responsesItems accepts both input shapes: an array of items, or a single
// string prompt.
func responsesItems(raw json.RawMessage) ([]responsesItem, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var items []responsesItem
	if err := json.Unmarshal(raw, &items); err == nil {
		return items, nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		encoded, _ := json.Marshal(single)
		return []responsesItem{{Type: "message", Role: "user", Content: encoded}}, nil
	}
	return nil, fmt.Errorf("decode responses input: unsupported shape")
}

// responsesContentToChat flattens Responses content (string or part array)
// into chat content.
func responsesContentToChat(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("decode responses content: %w", err)
	}
	out := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "output_text", "input_text", "text", "":
			out = append(out, map[string]any{"type": "text", "text": part.Text})
		case "input_image", "image_url":
			out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": part.ImageURL}})
		}
	}
	if len(out) == 0 {
		return "", nil
	}
	return out, nil
}

// strings_Trim trims surrounding whitespace.
func strings_Trim(v string) string {
	start, end := 0, len(v)
	for start < end && (v[start] == ' ' || v[start] == '\t' || v[start] == '\n' || v[start] == '\r') {
		start++
	}
	for end > start && (v[end-1] == ' ' || v[end-1] == '\t' || v[end-1] == '\n' || v[end-1] == '\r') {
		end--
	}
	return v[start:end]
}
