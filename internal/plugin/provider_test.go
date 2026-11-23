package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	tr "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestProviderRoutingAndOverrides(t *testing.T) {
	m := NewManager(nil)
	m.catalog.models = []catalogModel{{ID: "zai-org/GLM-5.3", SupportedEndpoints: []string{"/responses"}}}
	cfg := defaultConfig()
	for _, tt := range []struct{ id, path string }{{"claude-opus-5", "/messages"}, {"zai-org/GLM-5.3", "/responses"}, {"new-model", "/chat/completions"}} {
		_, path, err := m.providerWire(cfg, tt.id)
		if err != nil || path != tt.path {
			t.Fatalf("%s: %s %v", tt.id, path, err)
		}
	}
	cfg.EndpointOverrides = map[string]string{"zai-org/GLM-5.3": "chat-completions"}
	_, path, _ := m.providerWire(cfg, "zai-org/GLM-5.3")
	if path != "/chat/completions" {
		t.Fatal(path)
	}
	cfg.EndpointOverrides["new-model"] = "responses"
	if _, _, err := m.providerWire(cfg, "new-model"); err == nil {
		t.Fatal("unadvertised Responses accepted")
	}
	cfg.EndpointOverrides["claude-opus-5"] = "chat-completions"
	if _, _, err := m.providerWire(cfg, "claude-opus-5"); err == nil {
		t.Fatal("Claude override accepted")
	}
}

func TestNativeTranslationPreservesToolsAndImages(t *testing.T) {
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{SourceFormat: "claude", OriginalRequest: []byte(`{"system":"system prompt","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"x":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"found"}]}],"tools":[{"name":"lookup","description":"Find things","input_schema":{"type":"object"}}],"max_tokens":128}`)}}
	for _, wire := range []tr.Format{tr.FormatOpenAI, tr.FormatOpenAIResponse, tr.FormatClaude} {
		body, err := providerRequest(req, "test-model", wire)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []string{"YWJj", "call_1", "lookup", "found", "system prompt"} {
			if !strings.Contains(string(body), value) {
				t.Fatalf("%s lost %s: %s", wire, value, body)
			}
		}
	}
}

func TestResponsesRepairsAreModelScoped(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"hi"}],"reasoning":{"effort":"none"}}`)
	got := string(repairResponses(body, "zai-org/GLM-5.3"))
	if !strings.Contains(got, `"type":"message"`) || strings.Contains(got, "reasoning") {
		t.Fatal(got)
	}
	if string(repairResponses(body, "gpt-6.2-sol")) != string(body) {
		t.Fatal("other model changed")
	}
	body = []byte(`{"reasoning":{"effort":"none","summary":"auto"}}`)
	if string(repairResponses(body, "zai-org/GLM-5.3")) != string(body) {
		t.Fatal("active reasoning removed")
	}
	r := reasoningRepair{}
	event := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","encrypted_content":"thinking","summary":[]}}`)
	if len(r.feed(event, "gpt-6.2-sol")) != 1 {
		t.Fatal("ciphertext exposed")
	}
	if len(r.feed(event, "zai-org/GLM-5.3")) != 2 {
		t.Fatal("missing GLM reasoning")
	}
	if len(r.feed(event, "zai-org/GLM-5.3")) != 1 {
		t.Fatal("duplicate reasoning")
	}
}

func TestZDRAndCapabilities(t *testing.T) {
	t.Setenv("CMD_ZDR", "")
	t.Setenv("COMMANDCODE_ZDR", "")
	cfg := defaultConfig()
	if providerHeaders(cfg, "key", false, "").Get("x-cmd-zdr") != "" {
		t.Fatal("ZDR defaults on")
	}
	cfg.EnableZDR = true
	if providerHeaders(cfg, "key", false, "").Get("x-cmd-zdr") != "1" {
		t.Fatal("ZDR setting ignored")
	}
	cfg.EnableZDR = false
	t.Setenv("CMD_ZDR", "1")
	if providerHeaders(cfg, "key", false, "").Get("x-cmd-zdr") != "1" {
		t.Fatal("ZDR env ignored")
	}
	row := catalogModel{ID: "gpt-6.2-sol", ContextLength: 1000000}
	enrichCapabilities(&row)
	if !supportsImage(row) || len(row.ReasoningEfforts) == 0 || row.MaxOutputTokens != 65536 {
		t.Fatalf("%+v", row)
	}
	row = catalogModel{ID: "unknown", ContextLength: 4096}
	enrichCapabilities(&row)
	if row.MaxOutputTokens != 4096 {
		t.Fatal(row.MaxOutputTokens)
	}
}

func TestGoSnapshotEnrichesOnlyLiveRows(t *testing.T) {
	if len(goModels) != 62 {
		t.Fatal(len(goModels))
	}
	row := catalogModel{ID: "zai-org/GLM-5.3"}
	enrichCapabilities(&row)
	if row.MinimumPlan != "individual-go" {
		t.Fatal(row.MinimumPlan)
	}
	row.MinimumPlan = "individual-provider"
	enrichCapabilities(&row)
	if row.MinimumPlan != "individual-provider" {
		t.Fatal("live plan overwritten")
	}
}

func TestProviderStreamFramingAndResponseTranslation(t *testing.T) {
	for _, tt := range []struct{ model, source, wire, events, want string }{
		{"test-model", "openai", "", `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}` + "\n\n", "hello"},
		{"claude-opus-5", "claude", "", `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n", "event: content_block_delta\ndata:"},
		{"zai-org/GLM-5.3", "openai", "/responses", `data: {"type":"response.created","response":{"id":"resp_1","model":"zai-org/GLM-5.3","output":[]}}` + "\n\n" + `data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}` + "\n\n", "hello"},
	} {
		t.Run(tt.model, func(t *testing.T) {
			reads := 0
			var emitted strings.Builder
			closed := false
			m := NewManager(func(method string, raw []byte) ([]byte, error) {
				var result any = map[string]any{}
				switch method {
				case "host.http.stream_read":
					reads++
					mid := len(tt.events) / 2
					if reads == 1 {
						result = hostStreamReadResp{Payload: []byte(tt.events[:mid])}
					} else {
						result = hostStreamReadResp{Payload: []byte(tt.events[mid:]), Done: true}
					}
				case "host.stream.emit":
					var chunk struct {
						Payload []byte `json:"payload"`
					}
					_ = json.Unmarshal(raw, &chunk)
					emitted.Write(chunk.Payload)
				case "host.stream.close":
					closed = true
				}
				return json.Marshal(map[string]any{"ok": true, "result": result})
			})
			m.catalog.models = []catalogModel{{ID: tt.model, SupportedEndpoints: []string{tt.wire}}}
			req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: tt.model, SourceFormat: tt.source, Stream: true, OriginalRequest: []byte(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`)}, StreamID: "downstream"}
			m.pumpProviderStream(defaultConfig(), req, "upstream", req.OriginalRequest)
			if !closed || !strings.Contains(emitted.String(), tt.want) || !strings.HasSuffix(emitted.String(), "\n\n") {
				t.Fatalf("closed=%v stream=%s", closed, emitted.String())
			}
		})
	}
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "model", SourceFormat: "openai", OriginalRequest: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}}
	var state any
	output := providerResponse(req, []byte("{}"), []byte(`{"id":"resp_1","object":"response","status":"completed","model":"model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`), tr.FormatOpenAIResponse, &state)
	if !strings.Contains(string(output), "hello") || !strings.Contains(string(output), "choices") {
		t.Fatal(string(output))
	}
}

func TestEntitlementDenialNeverCallsGateway(t *testing.T) {
	for _, stream := range []bool{false, true} {
		calls := 0
		m := NewManager(func(method string, raw []byte) ([]byte, error) {
			var result any = map[string]any{}
			switch method {
			case "host.http.do", "host.http.do_stream":
				calls++
				var req hostHTTPReq
				_ = json.Unmarshal(raw, &req)
				if strings.Contains(req.URL, "/alpha/generate") {
					t.Fatal("private gateway invoked")
				}
				if stream {
					result = hostStreamStartResp{StatusCode: 403, StreamID: "denied"}
				} else {
					result = pluginapi.HTTPResponse{StatusCode: 403, Body: []byte(`{"error":{"code":"upgrade_required"}}`)}
				}
			case "host.http.stream_read":
				result = map[string]any{"payload": []byte(`{"error":{"code":"upgrade_required"}}`), "done": true}
			}
			return json.Marshal(map[string]any{"ok": true, "result": result})
		})
		cfg := defaultConfig()
		cfg.APIKeys = []string{"one", "two"}
		var err error
		if stream {
			_, _, _, _, err = m.executeStream(context.Background(), cfg, executorRequest{}, []byte("{}"), "https://example.com/chat/completions")
		} else {
			_, err = m.execute(context.Background(), cfg, executorRequest{}, []byte("{}"), "https://example.com/chat/completions")
		}
		if err == nil || calls != 1 {
			t.Fatalf("stream=%v calls=%d err=%v", stream, calls, err)
		}
		if code, ok := err.(interface{ StatusCode() int }); !ok || code.StatusCode() != http.StatusForbidden {
			t.Fatal(err)
		}
	}
}
