package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

// fakeCaller answers host.log and catalog requests so lifecycle can warm the
// catalog without touching the real network.
func fakeCaller(t *testing.T) rawCaller {
	t.Helper()
	return func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.log":
			return []byte("{\"ok\":true,\"result\":{}}"), nil
		case "host.http.do":
			var req struct {
				URL string `json:"url"`
			}
			_ = json.Unmarshal(payload, &req)
			if strings.Contains(req.URL, "/models") {
				body := []byte(`{"object":"list","data":[{"id":"deepseek/deepseek-v4.1-flash","object":"model","name":"DeepSeek V4.1 Flash","context_length":1000000,"minimumPlan":"individual-go","reasoningEfforts":["off","low","high","max"],"inputModalities":["text","image"],"zdr":true,"hidden":false,"supported_endpoints":["/chat/completions"]},{"id":"claude-opus-5","object":"model","name":"Claude Opus 5","context_length":1000000,"minimumPlan":"individual-provider","reasoningEfforts":["low","medium","high"],"inputModalities":["text"],"zdr":true,"hidden":false,"supported_endpoints":["/messages"]}]}`)
				result, _ := json.Marshal(map[string]any{"StatusCode": 200, "Headers": map[string][]string{"Content-Type": {"application/json"}}, "Body": body})
				return json.Marshal(map[string]any{"ok": true, "result": json.RawMessage(result)})
			}
			return []byte("{\"ok\":true,\"result\":{\"StatusCode\":404,\"Body\":\"\"}}"), nil
		default:
			return []byte("{\"ok\":true,\"result\":{}}"), nil
		}
	}
}

func TestRegisterDeclaresCapabilities(t *testing.T) {
	m := NewManager(fakeCaller(t))
	raw, err := m.HandleCall("plugin.register", nil)
	if err != nil {
		t.Fatalf("register handler error: %v", err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			SchemaVersion uint32 `json:"schema_version"`
			Capabilities  struct {
				ModelProvider      bool     `json:"model_provider"`
				AuthProvider       bool     `json:"auth_provider"`
				Executor           bool     `json:"executor"`
				ExecutorModelScope string   `json:"executor_model_scope"`
				InputFormats       []string `json:"executor_input_formats"`
				OutputFormats      []string `json:"executor_output_formats"`
				ManagementAPI      bool     `json:"management_api"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode register envelope: %v (%s)", err, raw)
	}
	if !env.OK {
		t.Fatalf("register not ok: %s", raw)
	}
	c := env.Result.Capabilities
	if !c.ModelProvider || !c.AuthProvider || !c.Executor || !c.ManagementAPI {
		t.Fatalf("capabilities missing: %+v", c)
	}
	if c.ExecutorModelScope != "both" {
		t.Fatalf("executor_model_scope = %q, want both", c.ExecutorModelScope)
	}
	if len(c.InputFormats) != 3 || len(c.OutputFormats) != 3 {
		t.Fatalf("formats = %v / %v, want 3 each", c.InputFormats, c.OutputFormats)
	}
}

func TestModelsFilteredByPlan(t *testing.T) {
	m := NewManager(fakeCaller(t))
	if _, err := m.HandleCall("plugin.register", nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	raw, err := m.HandleCall("model.static", nil)
	if err != nil {
		t.Fatalf("model.static: %v", err)
	}
	type modelRow struct {
		ID            string `json:"ID"`
		ContextLength int64  `json:"ContextLength"`
		DisplayName   string `json:"DisplayName"`
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Provider string     `json:"Provider"`
			Models   []modelRow `json:"Models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode models: %v (%s)", err, raw)
	}
	if !env.OK {
		t.Fatalf("model.static not ok: %s", raw)
	}
	if env.Result.Provider != ProviderID {
		t.Fatalf("provider = %q, want %q", env.Result.Provider, ProviderID)
	}
	if len(env.Result.Models) != 2 {
		t.Fatalf("models = %d, want 2: %s", len(env.Result.Models), raw)
	}
	// With plan-filter: go, only the Go-tier model survives.
	lifecycle, _ := json.Marshal(map[string]any{"config_yaml": []byte("plan-filter: go\n")})
	if _, err := m.HandleCall("plugin.reconfigure", lifecycle); err != nil {
		t.Fatalf("reconfigure: %v", err)
	}
	if got := m.snapshotConfig().PlanFilter; got != "individual-go" {
		t.Fatalf("PlanFilter after reconfigure = %q", got)
	}
	if rows := m.catalog.snapshot(); len(rows) != 2 {
		t.Fatalf("catalog rows after reconfigure = %d, want 2", len(rows))
	}
	raw, _ = m.HandleCall("model.static", nil)
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode models (go filter): %v (%s)", err, raw)
	}
	if len(env.Result.Models) != 1 {
		t.Fatalf("go filter models = %d, want 1: %s", len(env.Result.Models), raw)
	}
	if !strings.HasPrefix(env.Result.Models[0].ID, ProviderID+"/") {
		t.Fatalf("model id %q missing prefix", env.Result.Models[0].ID)
	}
}

func TestVisibleHonorsPlanFilter(t *testing.T) {
	cfg := defaultConfig()
	cfg.PlanFilter = "individual-go"
	rows := []catalogModel{
		{ID: "go", MinimumPlan: "individual-go"},
		{ID: "go-v1", MinimumPlan: "individual-go-v1"},
		{ID: "goat", MinimumPlan: "individual-goat"},
		{ID: "pro", MinimumPlan: "individual-pro"},
		{ID: "provider", MinimumPlan: "individual-provider"},
		{ID: "max", MinimumPlan: "individual-max"},
		{ID: "unknown", MinimumPlan: ""},
		{ID: "hidden", MinimumPlan: "individual-go", Hidden: true},
	}
	got := cfg.visible(rows)
	have := map[string]bool{}
	for _, row := range got {
		have[row.ID] = true
	}
	for _, want := range []string{"go", "go-v1"} {
		if !have[want] {
			t.Fatalf("plan-filter go dropped %q: got %v", want, have)
		}
	}
	for _, notWant := range []string{"goat", "pro", "provider", "max", "unknown", "hidden"} {
		if have[notWant] {
			t.Fatalf("plan-filter go leaked %q: got %v", notWant, have)
		}
	}
	if len(got) != 2 {
		t.Fatalf("plan-filter go kept %d rows, want 2", len(got))
	}
}

func TestVisibleKeepsRowsWithoutPlanMetadata(t *testing.T) {
	cfg := defaultConfig()
	cfg.PlanFilter = "individual-go"
	rows := []catalogModel{{ID: "a"}, {ID: "b"}}
	if got := cfg.visible(rows); len(got) != 2 {
		t.Fatalf("bare catalog fallback kept %d rows, want 2", len(got))
	}
}

func TestUnknownMethodIsErrorEnvelope(t *testing.T) {
	m := NewManager(fakeCaller(t))
	raw, err := m.HandleCall("does.not.exist", nil)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
		t.Fatalf("unexpected envelope: %s", raw)
	}
}

func TestTranslateOpenAIForcesModelAndStream(t *testing.T) {
	out, err := TranslateRequest("openai", []byte(`{"model":"commandcode/deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stop":["x"],"stream":false}`), "deepseek/deepseek-v4.1-flash", true)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["model"] != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("model = %v", payload["model"])
	}
	if payload["stream"] != true {
		t.Fatalf("stream = %v", payload["stream"])
	}
	if _, exists := payload["stop"]; exists {
		t.Fatalf("stop must be dropped, got %v", payload["stop"])
	}
	opts, _ := payload["stream_options"].(map[string]any)
	if opts == nil || opts["include_usage"] != true {
		t.Fatalf("stream_options = %v", payload["stream_options"])
	}
}

func TestTranslateClaudeRequest(t *testing.T) {
	original := []byte(`{"model":"claude-x","max_tokens":1024,"system":"be brief","messages":[{"role":"user","content":"hello"}],"tools":[{"name":"read","description":"d","input_schema":{"type":"object","properties":{}}}]}`)
	out, err := TranslateRequest("claude", original, "deepseek/deepseek-v4.1-flash", false)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["model"] != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("model = %v", payload["model"])
	}
	if payload["system"] != "be brief" {
		t.Fatalf("system = %v", payload["system"])
	}
	msgs, _ := payload["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", payload["messages"])
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", payload["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool == nil || tool["type"] != "function" || tool["name"] != "read" {
		t.Fatalf("tool = %v", tool)
	}
	if payload["max_tokens"] != float64(1024) {
		t.Fatalf("max_tokens = %v", payload["max_tokens"])
	}
}

func TestReasoningBackfill(t *testing.T) {
	in := []byte(`{"id":"x","choices":[{"delta":{"content":"hi"}}],"reasoning":"thinking out loud"}`)
	out, changed := mapReasoningBody(in)
	if !changed {
		t.Fatalf("expected a rewrite for %s", in)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload["reasoning_content"] != "thinking out loud" {
		t.Fatalf("reasoning_content = %v", payload["reasoning_content"])
	}
}

func TestAttemptOrderPrefersAuthKey(t *testing.T) {
	cfg := defaultConfig()
	cfg.APIKeys = []string{"k2", "k3"}
	req := executorRequest{}
	req.AuthAttributes = map[string]string{"api_key": "k1"}
	attempts := attemptOrder(cfg, req)
	if len(attempts) != 3 {
		t.Fatalf("attempts = %d, want 3", len(attempts))
	}
	if attempts[0].key != "k1" || !attempts[0].fromAuth {
		t.Fatalf("first attempt = %+v, want auth key first", attempts[0])
	}
	if attempts[1].key != "k2" || attempts[2].key != "k3" {
		t.Fatalf("pool order wrong: %+v", attempts)
	}
}

func TestRetryableStatusClassification(t *testing.T) {
	for _, status := range []int{401, 402, 403, 429, 500, 502, 503, 504} {
		if !isRetryableStatus(status) {
			t.Errorf("status %d should be retryable", status)
		}
	}
	for _, status := range []int{400, 404, 413, 422} {
		if isRetryableStatus(status) {
			t.Errorf("status %d should not be retryable", status)
		}
	}
}
