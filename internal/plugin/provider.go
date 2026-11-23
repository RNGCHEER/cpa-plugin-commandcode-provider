package plugin

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator/builtin"
	"github.com/tidwall/sjson"
)

// Generated capability snapshot from patlux/pi-commandcode-provider v0.8.7,
// command-code@1.91.1. IDs and endpoint availability still come from the live API.
//
//go:embed capabilities.json
var capabilitiesJSON []byte

// Official Go plan model IDs, retrieved 2026-11-23:
// https://commandcode.ai/docs/plans/go . Only enrich live catalog rows;
// never advertise a model that the API no longer lists.
//
//go:embed go-models.json
var goModelsJSON []byte
var goModels = func() map[string]bool {
	var ids []string
	if err := json.Unmarshal(goModelsJSON, &ids); err != nil {
		panic(err)
	}
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}()
var upstreamCapabilities = func() struct {
	Modalities map[string][]string `json:"MODEL_INPUT_MODALITIES"`
	Reasoning  map[string]bool     `json:"MODEL_REASONING"`
	Efforts    map[string][]string `json:"MODEL_EFFORTS"`
	Max        map[string]int64    `json:"MODEL_MAX_OUTPUT_TOKENS"`
} {
	var c struct {
		Modalities map[string][]string `json:"MODEL_INPUT_MODALITIES"`
		Reasoning  map[string]bool     `json:"MODEL_REASONING"`
		Efforts    map[string][]string `json:"MODEL_EFFORTS"`
		Max        map[string]int64    `json:"MODEL_MAX_OUTPUT_TOKENS"`
	}
	if err := json.Unmarshal(capabilitiesJSON, &c); err != nil {
		panic(err)
	}
	return c
}()

func enrichCapabilities(row *catalogModel) {
	if row.MinimumPlan == "" && goModels[row.ID] {
		row.MinimumPlan = "individual-go"
	}
	if len(row.InputModalities) == 0 {
		row.InputModalities = upstreamCapabilities.Modalities[row.ID]
	}
	if len(row.ReasoningEfforts) == 0 {
		row.ReasoningEfforts = upstreamCapabilities.Efforts[row.ID]
	}
	if row.MaxOutputTokens <= 0 {
		row.MaxOutputTokens = upstreamCapabilities.Max[row.ID]
		if row.MaxOutputTokens <= 0 {
			row.MaxOutputTokens = 65536
		}
	}
	if row.ContextLength > 0 && row.MaxOutputTokens > row.ContextLength {
		row.MaxOutputTokens = row.ContextLength
	}
}

func (m *Manager) providerWire(cfg config, id string) (tr.Format, string, error) {
	choice := cfg.EndpointOverrides[id]
	if strings.HasPrefix(id, "claude-") {
		if choice != "" && choice != "auto" {
			return "", "", fmt.Errorf("Claude models require Messages; use auto")
		}
		return tr.FormatClaude, "/messages", nil
	}
	responses := false
	if row, ok := m.catalog.lookup(id); ok {
		for _, ep := range row.SupportedEndpoints {
			if ep == "/responses" {
				responses = true
			}
		}
	}
	if choice == "responses" && !responses {
		return "", "", fmt.Errorf("catalog does not advertise Responses for %s", id)
	}
	if choice == "responses" || (responses && choice != "chat-completions") {
		return tr.FormatOpenAIResponse, "/responses", nil
	}
	return tr.FormatOpenAI, "/chat/completions", nil
}

func sourceWire(source string) tr.Format {
	switch source {
	case "", "openai", "chat-completions":
		return tr.FormatOpenAI
	case "responses", "openai-response":
		return tr.FormatOpenAIResponse
	case "claude":
		return tr.FormatClaude
	}
	return tr.Format(source)
}

func providerRequest(req executorRequest, id string, wire tr.Format) ([]byte, error) {
	from := sourceWire(req.SourceFormat)
	if wire == tr.FormatOpenAIResponse && from != wire {
		chat := req.OriginalRequest
		if from != tr.FormatOpenAI {
			chat = tr.TranslateRequest(from, tr.FormatOpenAI, id, chat, req.Stream)
		}
		return chatToResponses(chat, id, req.Stream)
	}
	bridge := from != wire && !tr.HasRequestTransformer(from, wire)
	if bridge && (!tr.HasRequestTransformer(from, tr.FormatOpenAI) || !tr.HasRequestTransformer(tr.FormatOpenAI, wire)) {
		return nil, fmt.Errorf("unsupported translation %s to %s", from, wire)
	}
	if !json.Valid(req.OriginalRequest) {
		return nil, fmt.Errorf("request body is not valid JSON")
	}
	original := req.OriginalRequest
	if bridge {
		original = tr.TranslateRequest(from, tr.FormatOpenAI, id, original, req.Stream)
		from = tr.FormatOpenAI
	}
	body := tr.TranslateRequest(from, wire, id, original, req.Stream)
	var err error
	body, err = sjson.SetBytes(body, "model", id)
	if err != nil {
		return nil, err
	}
	body, err = sjson.SetBytes(body, "stream", req.Stream)
	if err != nil {
		return nil, err
	}
	body, _ = sjson.DeleteBytes(body, "stop")
	body, _ = sjson.DeleteBytes(body, "stop_sequences")
	if wire == tr.FormatOpenAI && req.Stream {
		body, _ = sjson.SetBytes(body, "stream_options.include_usage", true)
	}
	if wire == tr.FormatOpenAIResponse {
		body = repairResponses(body, id)
	}
	return body, nil
}

func repairResponses(body []byte, id string) []byte {
	if id != "zai-org/GLM-5.3" && id != "mistral/mistral-large-4" {
		return body
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return body
	}
	changed := false
	if r, ok := obj["reasoning"].(map[string]any); ok && len(r) == 1 && r["effort"] == "none" {
		delete(obj, "reasoning")
		changed = true
	}
	if id == "zai-org/GLM-5.3" {
		if items, ok := obj["input"].([]any); ok {
			for _, v := range items {
				if item, ok := v.(map[string]any); ok && item["type"] == nil && item["content"] != nil {
					switch item["role"] {
					case "system", "developer", "user", "assistant":
						item["type"] = "message"
						changed = true
					}
				}
			}
		}
	}
	if !changed {
		return body
	}
	out, _ := json.Marshal(obj)
	return out
}

func providerHeaders(cfg config, key string, stream bool, url string) http.Header {
	h := upstreamHeaders(key, stream)
	if cfg.EnableZDR || os.Getenv("CMD_ZDR") == "1" || os.Getenv("COMMANDCODE_ZDR") == "1" {
		h.Set("x-cmd-zdr", "1")
	}
	if strings.HasSuffix(url, "/messages") {
		h.Set("x-api-key", key)
		h.Set("anthropic-version", "2023-06-01")
	}
	return h
}

func providerResponse(req executorRequest, body, payload []byte, wire tr.Format, state *any) []byte {
	if wire == tr.FormatOpenAIResponse && sourceWire(req.SourceFormat) != wire {
		var response struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(payload, &response)
		kind := "response.completed"
		if response.Status == "incomplete" {
			kind = "response.incomplete"
		}
		wrapped, _ := json.Marshal(map[string]any{"type": kind, "response": json.RawMessage(payload)})
		payload = wrapped
		wire = tr.FormatCodex
	}
	if target := sourceWire(req.SourceFormat); wire != target && !tr.HasNonStreamResponseTransformer(target, wire) {
		var intermediate any
		payload = tr.TranslateNonStream(context.Background(), wire, tr.FormatOpenAI, req.Model, req.OriginalRequest, body, payload, &intermediate)
		return tr.TranslateNonStream(context.Background(), tr.FormatOpenAI, target, req.Model, req.OriginalRequest, body, payload, state)
	}
	return tr.TranslateNonStream(context.Background(), wire, sourceWire(req.SourceFormat), req.Model, req.OriginalRequest, body, payload, state)
}

// reasoningRepair is scoped to GLM's known plaintext field. Other providers
// use encrypted_content for ciphertext; never expose it without the model gate.
type reasoningRepair struct{ seen map[int]bool }

func (r *reasoningRepair) feed(payload []byte, model string) [][]byte {
	if model != "zai-org/GLM-5.3" {
		return [][]byte{payload}
	}
	var e struct {
		Type  string `json:"type"`
		Index int    `json:"output_index"`
		Delta string `json:"delta"`
		Item  struct {
			Type    string            `json:"type"`
			Text    string            `json:"encrypted_content"`
			Summary []json.RawMessage `json:"summary"`
			Content []json.RawMessage `json:"content"`
		} `json:"item"`
	}
	if json.Unmarshal(payload, &e) != nil {
		return [][]byte{payload}
	}
	if r.seen == nil {
		r.seen = map[int]bool{}
	}
	if (e.Type == "response.reasoning_text.delta" || e.Type == "response.reasoning_summary_text.delta") && e.Delta != "" {
		r.seen[e.Index] = true
	}
	if e.Type == "response.output_item.done" && e.Item.Type == "reasoning" && e.Item.Text != "" && len(e.Item.Summary) == 0 && len(e.Item.Content) == 0 && !r.seen[e.Index] {
		r.seen[e.Index] = true
		delta, _ := json.Marshal(map[string]any{"type": "response.reasoning_text.delta", "output_index": e.Index, "delta": e.Item.Text})
		return [][]byte{delta, payload}
	}
	return [][]byte{payload}
}

func (m *Manager) pumpProviderStream(cfg config, req executorRequest, upstreamID string, body []byte) {
	var finalError string
	defer func() {
		_ = m.bridge.streamClose(upstreamID)
		_ = m.bridge.streamCloseDownstream(req.StreamID, finalError)
	}()
	wire, _, _ := m.providerWire(cfg, m.catalogIDFor(req.Model))
	target := sourceWire(req.SourceFormat)
	if wire == tr.FormatOpenAIResponse && target != wire {
		wire = tr.FormatCodex
	}
	var state any
	var intermediate any
	var pending []byte
	repair := reasoningRepair{}
	emit := func(payload []byte) bool {
		for _, fixed := range repair.feed(payload, m.catalogIDFor(req.Model)) {
			if wire == tr.FormatOpenAI {
				fixed = backfillChunk(fixed)
			}
			input := append([]byte("data: "), fixed...)
			outputs := [][]byte{input}
			from := wire
			if wire != target && !tr.HasStreamResponseTransformer(target, wire) {
				outputs = tr.TranslateStream(context.Background(), wire, tr.FormatOpenAI, req.Model, req.OriginalRequest, body, input, &intermediate)
				from = tr.FormatOpenAI
			}
			var translated [][]byte
			for _, part := range outputs {
				translated = append(translated, tr.TranslateStream(context.Background(), from, target, req.Model, req.OriginalRequest, body, part, &state)...)
			}
			for _, out := range translated {
				if len(out) == 0 {
					continue
				}
				if !bytes.HasPrefix(bytes.TrimSpace(out), []byte("data:")) && !bytes.HasPrefix(bytes.TrimSpace(out), []byte("event:")) {
					out = append(append([]byte("data: "), out...), []byte("\n\n")...)
				}
				if !bytes.HasSuffix(out, []byte("\n\n")) {
					out = append(bytes.TrimRight(out, "\r\n"), []byte("\n\n")...)
				}
				if target == tr.FormatClaude && !bytes.HasPrefix(bytes.TrimSpace(out), []byte("event:")) {
					var event struct {
						Type string `json:"type"`
					}
					_ = json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(out), []byte("data:"))), &event)
					if event.Type != "" {
						out = append([]byte("event: "+event.Type+"\n"), out...)
					}
				}
				if err := m.bridge.streamEmit(req.StreamID, out); err != nil {
					finalError = redact(err.Error())
					return false
				}
			}
		}
		return true
	}
	for {
		chunk, err := m.bridge.streamRead(upstreamID)
		if err != nil {
			finalError = redact(err.Error())
			return
		}
		if chunk.Error != "" {
			finalError = redact(chunk.Error)
			return
		}
		pending = append(pending, chunk.Payload...)
		if chunk.Done {
			pending = append(pending, '\n')
		}
		lines, rest := parseStreamLines(pending)
		pending = rest
		for _, line := range lines {
			if !emit(line.payload) {
				return
			}
		}
		if chunk.Done {
			if target == tr.FormatOpenAI {
				if err := m.bridge.streamEmit(req.StreamID, []byte("data: [DONE]\n\n")); err != nil {
					finalError = redact(err.Error())
				}
			}
			return
		}
	}
}
