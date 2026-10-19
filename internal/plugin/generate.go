package plugin

// The CommandCode Provider API (https://api.commandcode.ai/provider/v1) only
// serves accounts on the paid "Provider" tiers. A Go / GOAT / Pro subscription
// is rejected there with HTTP 403 {"error":{"code":"upgrade_required"}} and can
// only be used through Command Code's private CLI gateway, POST
// /alpha/generate. That gateway speaks the CLI's own envelope and streams
// newline-delimited JSON events instead of OpenAI SSE chunks, so this file
// carries both the request translation (chat-completions -> CLI envelope) and
// the response translation (CLI events -> OpenAI chat-completions), mirroring
// the upstream providers:
//
//   - https://github.com/patlux/pi-commandcode-provider (transport router +
//     converters.ts message/tool translation)
//   - https://github.com/Mars-Sea/dsh-commandcode-provider (the /alpha/generate
//     protocol: config/params envelope and CLI stream events)

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

const (
	// defaultGenerateMaxTokens mirrors the CLI's own default output budget.
	defaultGenerateMaxTokens = 64000
	// generateToolCallIDLimit is the gateway's maximum call_id length; longer
	// ids are rejected with 400 input[N].call_id.
	generateToolCallIDLimit = 64
)

// cliEvent is one line of the /alpha/generate event stream. Only the fields
// this port consumes are declared; unknown event types are ignored.
type cliEvent struct {
	Type string `json:"type"`
	ID   string `json:"id"`

	Text  string `json:"text"`
	Delta string `json:"delta"`

	ToolName   string          `json:"toolName"`
	ToolCallID string          `json:"toolCallId"`
	Input      json.RawMessage `json:"input"`

	FinishReason    string    `json:"finishReason"`
	RawFinishReason string    `json:"rawFinishReason"`
	Usage           *cliUsage `json:"usage"`
	TotalUsage      *cliUsage `json:"totalUsage"`

	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
}

// cliUsage is the gateway's token accounting.
type cliUsage struct {
	InputTokens       int `json:"inputTokens"`
	OutputTokens      int `json:"outputTokens"`
	TotalTokens       int `json:"totalTokens"`
	InputTokenDetails struct {
		NoCacheTokens   int `json:"noCacheTokens"`
		CacheReadTokens int `json:"cacheReadTokens"`
	} `json:"inputTokenDetails"`
}

// chatRequest is the subset of the translated chat-completions payload the CLI
// envelope needs. Everything else (response_format, tool_choice, ...) is
// dropped because the gateway's schema rejects unknown params.
type chatRequest struct {
	Messages        []json.RawMessage `json:"messages"`
	Tools           []json.RawMessage `json:"tools"`
	System          json.RawMessage   `json:"system"`
	MaxTokens       *int              `json:"max_tokens"`
	Temperature     *float64          `json:"temperature"`
	ReasoningEffort string            `json:"reasoning_effort"`
}

// chatMessage is one OpenAI chat message as the client sent it.
type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`

	// Reasoning replayed from a prior assistant turn, under either spelling a
	// host may use.
	ReasoningContent string `json:"reasoning_content"`
	Reasoning        string `json:"reasoning"`

	IsError *bool `json:"is_error"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// cliConfig / cliParams / cliEnvelope are the gateway's request envelope.
type cliConfig struct {
	WorkingDir    string `json:"workingDir"`
	Date          string `json:"date"`
	Environment   string `json:"environment"`
	Structure     []any  `json:"structure"`
	IsGitRepo     bool   `json:"isGitRepo"`
	CurrentBranch string `json:"currentBranch"`
	MainBranch    string `json:"mainBranch"`
	GitStatus     string `json:"gitStatus"`
	RecentCommits []any  `json:"recentCommits"`
}

type cliParams struct {
	Model           string   `json:"model"`
	Messages        []any    `json:"messages"`
	Tools           []any    `json:"tools"`
	System          string   `json:"system"`
	MaxTokens       int      `json:"max_tokens"`
	Stream          bool     `json:"stream"`
	Temperature     *float64 `json:"temperature,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
}

type cliEnvelope struct {
	Config   cliConfig `json:"config"`
	Memory   any       `json:"memory"`
	Taste    any       `json:"taste"`
	Skills   any       `json:"skills"`
	Params   cliParams `json:"params"`
	ThreadID string    `json:"threadId"`
}

// ---- endpoints ---------------------------------------------------------------

// providerRoot strips the OpenAI-compatible surface off the configured base
// URL so the CLI gateway (/alpha/*) can be addressed on the same authority.
func providerRoot(baseURL string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	trimmed = strings.TrimSuffix(trimmed, "/provider/v1")
	trimmed = strings.TrimSuffix(trimmed, "/provider")
	if !strings.HasPrefix(trimmed, "https://") && !strings.HasPrefix(trimmed, "http://") {
		return "", fmt.Errorf("commandcode: base-url is not an absolute URL")
	}
	return trimmed, nil
}

// generateURL resolves the CLI gateway used for Go/GOAT/Pro accounts.
func generateURL(cfg config) (string, error) {
	if trimmed := strings.TrimSpace(cfg.GenerateURL); trimmed != "" {
		return trimmed, nil
	}
	root, err := providerRoot(cfg.BaseURL)
	if err != nil {
		return "", err
	}
	return root + "/alpha/generate", nil
}

// isUpgradeRequired reports the Provider API's "this plan has no API access"
// rejection, which is the only signal that the CLI gateway exists for this
// account.
func isUpgradeRequired(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	var nested struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &nested); err == nil && nested.Error.Code == "upgrade_required" {
		return true
	}
	var bare struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &bare); err == nil && bare.Code == "upgrade_required" {
		return true
	}
	return bytes.Contains(body, []byte("upgrade_required"))
}

// generateHeaders builds the per-key CLI gateway headers. The version header
// must track the published CLI (see CLIProxyAPI issue #3955), so it is
// configurable and defaults to the release this port was verified against.
func generateHeaders(cfg config, key string, stream bool) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "cli")
	h.Set("x-command-code-version", cfg.CLIVersion)
	h.Set("x-cli-environment", "production")
	h.Set("x-project-slug", projectSlug(cfg.WorkingDir))
	h.Set("x-taste-learning", "true")
	if key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	return h
}

// projectSlug mirrors the CLI's own working-directory slug.
func projectSlug(dir string) string {
	slug := strings.ToLower(strings.TrimSpace(dir))
	if slug == "" {
		return "cliproxyapi"
	}
	out := make([]byte, 0, len(slug))
	dash := false
	for i := 0; i < len(slug); i++ {
		c := slug[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
			dash = false
		default:
			if !dash && len(out) > 0 {
				out = append(out, '-')
				dash = true
			}
		}
	}
	if trimmed := strings.Trim(string(out), "-"); trimmed != "" {
		return trimmed
	}
	return "cliproxyapi"
}

// newThreadID returns a v4 UUID for the gateway's threadId.
func newThreadID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("cpa-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	buf := make([]byte, 36)
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf)
}

// ---- request translation -----------------------------------------------------

// buildGenerateBody rewrites one translated chat-completions payload into the
// CLI envelope. modelMaxOutput, when > 0, clamps the output budget to the
// catalog's advertised maximum.
func buildGenerateBody(chatBody []byte, cfg config, modelID string, modelMaxOutput int) ([]byte, error) {
	if len(chatBody) == 0 {
		return nil, fmt.Errorf("commandcode: empty request body")
	}
	var req chatRequest
	if err := json.Unmarshal(chatBody, &req); err != nil {
		return nil, fmt.Errorf("commandcode: decode chat request: %w", err)
	}

	messages, systemText := convertMessages(req.Messages)
	systemText = joinSystem(stringFromJSON(req.System), systemText)

	maxTokens := defaultGenerateMaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}
	if modelMaxOutput > 0 && maxTokens > modelMaxOutput {
		maxTokens = modelMaxOutput
	}

	workingDir := strings.TrimSpace(cfg.WorkingDir)
	if workingDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			workingDir = cwd
		} else {
			workingDir = "/"
		}
	}

	envelope := cliEnvelope{
		Config: cliConfig{
			WorkingDir:    workingDir,
			Date:          time.Now().Format("2006-01-02"),
			Environment:   runtime.GOOS + "-" + runtime.GOARCH,
			Structure:     []any{},
			RecentCommits: []any{},
		},
		Params: cliParams{
			Model:           modelID,
			Messages:        messages,
			Tools:           convertTools(req.Tools),
			System:          systemText,
			MaxTokens:       maxTokens,
			Stream:          true,
			Temperature:     req.Temperature,
			ReasoningEffort: strings.TrimSpace(req.ReasoningEffort),
		},
		ThreadID: newThreadID(),
	}
	return json.Marshal(envelope)
}

func stringFromJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Block-form system prompts flatten to their text parts.
	return textOfContent(raw)
}

func joinSystem(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return strings.Join(out, "\n\n")
}

// textOfContent flattens an OpenAI content value (string, block array, or null)
// into plain text.
func textOfContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text", "output_text", "":
			out = append(out, part.Text)
		}
	}
	return strings.Join(out, "")
}

// convertTools maps OpenAI tools onto the CLI's tool schema. Non-function
// entries (server-side built-ins) are passed through verbatim.
func convertTools(raw []json.RawMessage) []any {
	out := make([]any, 0, len(raw))
	for _, entry := range raw {
		var probe struct {
			Type     string `json:"type"`
			Function *struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal(entry, &probe); err != nil {
			continue
		}
		schema := json.RawMessage(`{"type":"object"}`)
		name, description := "", ""
		if probe.Function != nil {
			name = probe.Function.Name
			description = probe.Function.Description
			if len(probe.Function.Parameters) > 0 && string(probe.Function.Parameters) != "null" {
				schema = probe.Function.Parameters
			}
		} else if probe.Name != "" {
			// Already CLI-shaped (or a Responses-style entry).
			name = probe.Name
			description = probe.Description
			if len(probe.InputSchema) > 0 && string(probe.InputSchema) != "null" {
				schema = probe.InputSchema
			}
		}
		if name == "" {
			// Server-side tools such as web_search carry no function payload.
			out = append(out, json.RawMessage(entry))
			continue
		}
		out = append(out, map[string]any{
			"type":         "function",
			"name":         name,
			"description":  description,
			"input_schema": schema,
		})
	}
	return out
}

// convertMessages rewrites OpenAI chat messages onto the CLI's message union:
//
//	user      -> {role:"user",      content: string | [{type:"text"|"image"}]}
//	assistant -> {role:"assistant", content: [{type:"text"|"reasoning"|"tool-call"}]}
//	tool      -> {role:"tool",      content: [{type:"tool-result", ...}]}
//
// System turns are hoisted out into the envelope's params.system (the gateway
// rejects them inside messages). Consecutive tool results merge into one tool
// turn, and an assistant tool call with no matching result gains a synthetic
// error result, both matching the upstream ports.
func convertMessages(raw []json.RawMessage) ([]any, string) {
	msgs := make([]chatMessage, 0, len(raw))
	for _, entry := range raw {
		var msg chatMessage
		if err := json.Unmarshal(entry, &msg); err != nil {
			continue
		}
		msgs = append(msgs, msg)
	}

	// The gateway caps call_id at 64 characters, so compound host ids are
	// shortened deterministically while preserving pairing.
	wireIDs := generateToolCallIDMap(msgs)
	wireID := func(id string) string {
		if mapped, ok := wireIDs[id]; ok {
			return mapped
		}
		return id
	}
	// Resolve tool names for tool-result turns, which OpenAI carries by id only.
	toolNames := map[string]string{}
	for _, msg := range msgs {
		for _, call := range msg.ToolCalls {
			if call.ID != "" {
				toolNames[call.ID] = call.Function.Name
			}
		}
	}
	answered := map[string]bool{}
	for _, msg := range msgs {
		if msg.Role == "tool" && msg.ToolCallID != "" {
			answered[msg.ToolCallID] = true
		}
	}

	out := make([]any, 0, len(msgs))
	systems := make([]string, 0, 2)
	for i := 0; i < len(msgs); i++ {
		msg := msgs[i]
		switch msg.Role {
		case "system":
			if text := strings.TrimSpace(textOfContent(msg.Content)); text != "" {
				systems = append(systems, text)
			}

		case "developer", "user":
			out = append(out, map[string]any{"role": "user", "content": userContent(msg.Content)})

		case "assistant":
			parts := make([]any, 0, 4)
			synthetic := make([]any, 0, 2)
			if reasoning := firstNonBlank(msg.ReasoningContent, msg.Reasoning); reasoning != "" {
				// The CLI replays prior thinking; dropping it breaks the thinking
				// models' multi-turn loops.
				parts = append(parts, map[string]any{"type": "reasoning", "text": reasoning})
			}
			if text := textOfContent(msg.Content); text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			for _, call := range msg.ToolCalls {
				if call.ID == "" {
					continue
				}
				id := wireID(call.ID)
				parts = append(parts, map[string]any{
					"type":       "tool-call",
					"toolCallId": id,
					"toolName":   call.Function.Name,
					"input":      recordOrEmpty(call.Function.Arguments),
				})
				if !answered[call.ID] {
					synthetic = append(synthetic, map[string]any{
						"type":       "tool-result",
						"toolCallId": id,
						"toolName":   call.Function.Name,
						"output": map[string]any{
							"type":  "error-text",
							"value": "No result — the tool call did not complete (interrupted or lost).",
						},
					})
				}
			}
			if len(parts) > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": parts})
			}
			if len(synthetic) > 0 {
				out = append(out, map[string]any{"role": "tool", "content": synthetic})
			}

		case "tool":
			// Merge consecutive tool results into one tool turn.
			results := make([]any, 0, 2)
			images := make([]any, 0, 1)
			j := i
			for ; j < len(msgs) && msgs[j].Role == "tool"; j++ {
				entry := msgs[j]
				output := map[string]any{"type": "text", "value": textOfContent(entry.Content)}
				if entry.IsError != nil && *entry.IsError {
					output = map[string]any{"type": "error-text", "value": textOfContent(entry.Content)}
				}
				name := entry.Name
				if name == "" {
					name = toolNames[entry.ToolCallID]
				}
				results = append(results, map[string]any{
					"type":       "tool-result",
					"toolCallId": wireID(entry.ToolCallID),
					"toolName":   name,
					"output":     output,
				})
				images = append(images, imagePartsOf(entry.Content)...)
			}
			i = j - 1
			if len(results) > 0 {
				out = append(out, map[string]any{"role": "tool", "content": results})
			}
			if len(images) > 0 {
				out = append(out, map[string]any{"role": "user", "content": images})
			}

		default:
			// Unknown roles degrade to a user turn rather than being dropped.
			if text := textOfContent(msg.Content); text != "" {
				out = append(out, map[string]any{"role": "user", "content": text})
			}
		}
	}
	return out, strings.Join(systems, "\n\n")
}

// userContent renders an OpenAI user content value for the CLI.
func userContent(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return textOfContent(raw)
	}
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text", "":
			out = append(out, map[string]any{"type": "text", "text": part.Text})
		case "image_url", "input_image":
			out = append(out, encodeImage(part.ImageURL.URL))
		}
	}
	if len(out) == 0 {
		return textOfContent(raw)
	}
	return out
}

// imagePartsOf extracts image blocks from a tool result's content so they can
// be replayed as a separate follow-up user turn.
func imagePartsOf(raw json.RawMessage) []any {
	var parts []struct {
		Type     string `json:"type"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil
	}
	out := make([]any, 0, 1)
	for _, part := range parts {
		if part.Type == "image_url" || part.Type == "input_image" {
			out = append(out, encodeImage(part.ImageURL.URL))
		}
	}
	return out
}

// encodeImage converts an OpenAI image URL (data URL or remote) to the CLI's
// image block.
func encodeImage(url string) any {
	mimeType := "image/png"
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		if idx := strings.Index(rest, ";base64,"); idx >= 0 {
			mimeType = rest[:idx]
		}
	}
	return map[string]any{"type": "image", "image": url, "mimeType": mimeType}
}

// recordOrEmpty parses the OpenAI tool-argument string into the object shape
// the CLI's tool-call input requires.
func recordOrEmpty(arguments string) any {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return map[string]any{}
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return map[string]any{"_raw": trimmed}
	}
	if decoded == nil {
		return map[string]any{}
	}
	return decoded
}

// generateToolCallIDMap maps every tool-call id this request mentions onto its
// gateway-safe form (<= generateToolCallIDLimit characters). Short ids pass
// through; overlong ones are shortened with a stable hash suffix. Pairing
// between calls and results is preserved because both sides share the map.
func generateToolCallIDMap(msgs []chatMessage) map[string]string {
	referenced := make([]string, 0, 4)
	for _, msg := range msgs {
		for _, call := range msg.ToolCalls {
			if call.ID != "" {
				referenced = append(referenced, call.ID)
			}
		}
		if msg.Role == "tool" && msg.ToolCallID != "" {
			referenced = append(referenced, msg.ToolCallID)
		}
	}
	mapped := map[string]string{}
	used := map[string]bool{}
	for _, id := range referenced {
		if len(id) <= generateToolCallIDLimit {
			mapped[id] = id
			used[id] = true
		}
	}
	for _, id := range referenced {
		if _, ok := mapped[id]; ok {
			continue
		}
		candidate := shortenToolCallID(id)
		for attempt := 0; used[candidate]; attempt++ {
			candidate = shortenToolCallID(fmt.Sprintf("%s#%d", id, attempt))
		}
		mapped[id] = candidate
		used[candidate] = true
	}
	return mapped
}

func shortenToolCallID(value string) string {
	if len(value) <= generateToolCallIDLimit {
		return value
	}
	suffix := hashSuffix(value)
	head := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			head = append(head, c)
		default:
			head = append(head, '_')
		}
	}
	trimmed := strings.TrimRight(string(head), "_")
	keep := generateToolCallIDLimit - len(suffix) - 1
	if keep < 0 {
		keep = 0
	}
	if keep > len(trimmed) {
		keep = len(trimmed)
	}
	if trimmed = strings.TrimRight(trimmed[:keep], "_"); trimmed != "" {
		return trimmed + "_" + suffix
	}
	return suffix
}

// hashSuffix is a stable 8-hex fingerprint of a tool-call id.
func hashSuffix(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:4])
}

// firstNonBlank returns the first value that carries non-whitespace content.
func firstNonBlank(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
