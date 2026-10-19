package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsUpgradeRequired(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"nested", 403, `{"error":{"code":"upgrade_required","message":"Your Go plan doesn't include API access."}}`, true},
		{"bare", 403, `{"code":"upgrade_required"}`, true},
		{"substring", 403, `upgrade_required`, true},
		{"other 403", 403, `{"error":{"code":"forbidden"}}`, false},
		{"other status", 429, `{"error":{"code":"upgrade_required"}}`, false},
	}
	for _, tc := range cases {
		if got := isUpgradeRequired(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: isUpgradeRequired(%d, %s) = %v, want %v", tc.name, tc.status, tc.body, got, tc.want)
		}
	}
}

func TestBuildGenerateBody(t *testing.T) {
	chat := []byte(`{
		"model":"deepseek/deepseek-v4-flash",
		"stream":true,
		"reasoning_effort":"high",
		"max_tokens":99999,
		"messages":[
			{"role":"system","content":"be terse"},
			{"role":"user","content":"hi"}
		],
		"tools":[{"type":"function","function":{"name":"get_time","description":"d","parameters":{"type":"object","properties":{}}}}]
	}`)

	cfg := defaultConfig()
	raw, err := buildGenerateBody(chat, cfg, "deepseek/deepseek-v4-flash", 4096)
	if err != nil {
		t.Fatalf("buildGenerateBody: %v", err)
	}

	var env struct {
		Config struct {
			WorkingDir    string `json:"workingDir"`
			Date          string `json:"date"`
			Environment   string `json:"environment"`
			Structure     []any  `json:"structure"`
			RecentCommits []any  `json:"recentCommits"`
		} `json:"config"`
		Params struct {
			Model           string            `json:"model"`
			System          string            `json:"system"`
			MaxTokens       int               `json:"max_tokens"`
			Stream          bool              `json:"stream"`
			ReasoningEffort string            `json:"reasoning_effort"`
			Messages        []json.RawMessage `json:"messages"`
			Tools           []struct {
				Type        string          `json:"type"`
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"input_schema"`
			} `json:"tools"`
		} `json:"params"`
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}

	if env.Params.System != "be terse" {
		t.Errorf("system = %q, want %q", env.Params.System, "be terse")
	}
	if len(env.Params.Messages) != 1 {
		t.Fatalf("system turn must be hoisted out of messages, got %d", len(env.Params.Messages))
	}
	if got := string(env.Params.Messages[0]); !strings.Contains(got, `"role":"user"`) {
		t.Errorf("remaining message = %s", got)
	}
	if env.Params.MaxTokens != 4096 {
		t.Errorf("max_tokens = %d, want it clamped to the catalog limit 4096", env.Params.MaxTokens)
	}
	if env.Params.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("model = %q", env.Params.Model)
	}
	if !env.Params.Stream {
		t.Error("stream must stay true: the plugin parses the gateway event stream")
	}
	if env.Params.ReasoningEffort != "high" {
		t.Errorf("reasoning_effort = %q", env.Params.ReasoningEffort)
	}
	if len(env.Params.Tools) != 1 || env.Params.Tools[0].Name != "get_time" || env.Params.Tools[0].Type != "function" {
		t.Errorf("tools = %+v", env.Params.Tools)
	}
	if len(env.Params.Tools[0].InputSchema) == 0 {
		t.Error("tool input_schema must be carried through")
	}
	if env.ThreadID == "" {
		t.Error("threadId must be set")
	}
	if env.Config.Environment == "" || env.Config.Date == "" || env.Config.WorkingDir == "" {
		t.Errorf("config = %+v", env.Config)
	}
	// The gateway validates config.environment as a string; an object is a 400.
	if !strings.Contains(string(raw), `"environment":"`) {
		t.Errorf("environment must encode as a JSON string: %s", raw)
	}
}

func TestConvertMessagesToolRoundTrip(t *testing.T) {
	raw := []json.RawMessage{
		json.RawMessage(`{"role":"assistant","reasoning_content":"weighing options","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"utc\"}"}}]}`),
		json.RawMessage(`{"role":"tool","tool_call_id":"call_a","content":"12:00"}`),
		json.RawMessage(`{"role":"assistant","tool_calls":[{"id":"call_b","type":"function","function":{"name":"unfinished","arguments":""}}]}`),
	}

	msgs, system := convertMessages(raw)
	if system != "" {
		t.Errorf("no system turns were sent, got %q", system)
	}
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (assistant, tool, assistant, synthetic tool): %+v", len(msgs), msgs)
	}

	first := mustMap(t, msgs[0])
	if first["role"] != "assistant" {
		t.Errorf("msgs[0].role = %v", first["role"])
	}
	parts := anySlice(t, first["content"])
	if len(parts) != 2 {
		t.Fatalf("assistant parts = %d, want reasoning + tool-call: %+v", len(parts), parts)
	}
	if p := mustMap(t, parts[0]); p["type"] != "reasoning" || p["text"] != "weighing options" {
		t.Errorf("assistant part[0] = %+v", p)
	}
	call := mustMap(t, parts[1])
	if call["type"] != "tool-call" || call["toolName"] != "get_time" || call["toolCallId"] != "call_a" {
		t.Errorf("assistant part[1] = %+v", call)
	}
	if args := mustMap(t, call["input"]); args["tz"] != "utc" {
		t.Errorf("tool-call input = %+v", args)
	}

	res := mustMap(t, msgs[1])
	if res["role"] != "tool" {
		t.Errorf("msgs[1].role = %v", res["role"])
	}
	result := mustMap(t, anySlice(t, res["content"])[0])
	if result["type"] != "tool-result" || result["toolCallId"] != "call_a" {
		t.Errorf("tool result = %+v", result)
	}
	output := mustMap(t, result["output"])
	if output["type"] != "text" || output["value"] != "12:00" {
		t.Errorf("tool output = %+v", output)
	}

	// The second assistant turn has an unanswered tool call, so a synthetic
	// error result keeps the gateway's call/result pairing intact.
	if role := mustMap(t, msgs[2])["role"]; role != "assistant" {
		t.Errorf("msgs[2].role = %v", role)
	}
	synthetic := mustMap(t, anySlice(t, mustMap(t, msgs[3])["content"])[0])
	if synthetic["type"] != "tool-result" || synthetic["toolCallId"] != "call_b" {
		t.Errorf("synthetic result = %+v", synthetic)
	}
	if kind := mustMap(t, synthetic["output"])["type"]; kind != "error-text" {
		t.Errorf("synthetic output type = %v, want error-text", kind)
	}
}

func TestCLITranslatorStreamsReasoningTextAndUsage(t *testing.T) {
	tr := newCLITranslator("commandcode/deepseek/deepseek-v4-flash", true)
	chunks := feedAll(t, tr,
		`{"type":"start"}`,
		`{"type":"reasoning-start","id":"reasoning-0"}`,
		`{"type":"reasoning-delta","id":"reasoning-0","text":"why"}`,
		`{"type":"reasoning-end","id":"reasoning-0"}`,
		`{"type":"text-start","id":"txt-0"}`,
		`{"type":"text-delta","id":"txt-0","text":"hi"}`,
		`{"type":"text-delta","id":"txt-0","text":" there"}`,
		`{"type":"text-end","id":"txt-0"}`,
		`{"type":"finish","finishReason":"stop","rawFinishReason":"stop","totalUsage":{"inputTokens":7,"inputTokenDetails":{"cacheReadTokens":2},"outputTokens":3,"totalTokens":10}}`,
	)
	joined := strings.Join(chunks, "\n")
	for _, want := range []string{
		`"reasoning_content":"why"`,
		`"content":"hi"`,
		`"content":" there"`,
		`"finish_reason":"stop"`,
		`"prompt_tokens":7`,
		`"completion_tokens":3`,
		`"total_tokens":10`,
		`"cached_tokens":2`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("stream missing %s\n%s", want, joined)
		}
	}
	assertChunkShapes(t, chunks)
}

func TestCLITranslatorStreamsToolCalls(t *testing.T) {
	tr := newCLITranslator("m", true)
	chunks := feedAll(t, tr,
		`{"type":"reasoning-delta","text":"pick a tool"}`,
		`{"type":"tool-input-start","id":"call_00_abc","toolName":"get_time","dynamic":false}`,
		`{"type":"tool-input-delta","id":"call_00_abc","delta":"{}"}`,
		`{"type":"tool-input-end","id":"call_00_abc"}`,
		`{"type":"tool-call","toolCallId":"call_00_abc","toolName":"get_time","input":{}}`,
		`{"type":"finish","finishReason":"tool-calls","totalUsage":{"inputTokens":5,"outputTokens":2,"totalTokens":7}}`,
	)
	joined := strings.Join(chunks, "\n")
	for _, want := range []string{
		`"name":"get_time"`,
		`"arguments":"{}"`,
		`"id":"call_00_abc"`,
		`"finish_reason":"tool_calls"`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("stream missing %s\n%s", want, joined)
		}
	}
	assertChunkShapes(t, chunks)
}

func TestAggregateGenerateResponse(t *testing.T) {
	body := []byte(`{"type":"start"}
{"type":"reasoning-delta","text":"hmm"}
{"type":"text-delta","text":"hello"}
{"type":"text-delta","text":" world"}
{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}
`)

	out, err := aggregateGenerateResponse(body, "m")
	if err != nil {
		t.Fatalf("aggregateGenerateResponse: %v", err)
	}
	var got struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if got.Object != "chat.completion" {
		t.Errorf("object = %q", got.Object)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d", len(got.Choices))
	}
	if got.Choices[0].Message.Content != "hello world" {
		t.Errorf("content = %q", got.Choices[0].Message.Content)
	}
	if got.Choices[0].Message.ReasoningContent != "hmm" {
		t.Errorf("reasoning_content = %q", got.Choices[0].Message.ReasoningContent)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", got.Choices[0].FinishReason)
	}
	if got.Usage.TotalTokens != 3 {
		t.Errorf("total_tokens = %d", got.Usage.TotalTokens)
	}
}

func TestGenerateToolCallIDsStayPairedAndBounded(t *testing.T) {
	long := strings.Repeat("x", 80)
	msgs := []chatMessage{
		{ToolCalls: []chatToolCall{{ID: long}}},
		{Role: "tool", ToolCallID: long},
		{ToolCalls: []chatToolCall{{ID: "call_short"}}},
	}
	mapped := generateToolCallIDMap(msgs)

	if got := mapped[long]; len(got) > generateToolCallIDLimit {
		t.Errorf("shortened id is %d chars, want <= %d", len(got), generateToolCallIDLimit)
	}
	if mapped["call_short"] != "call_short" {
		t.Errorf("short ids must pass through, got %q", mapped["call_short"])
	}
	if mapped[long] == "" {
		t.Error("every referenced id must map")
	}
}

func TestProviderRootStripsProviderSurface(t *testing.T) {
	got, err := providerRoot("https://api.commandcode.ai/provider/v1")
	if err != nil {
		t.Fatalf("providerRoot: %v", err)
	}
	if got != "https://api.commandcode.ai" {
		t.Errorf("providerRoot = %q", got)
	}
	if _, err := providerRoot("api.commandcode.ai/provider/v1"); err == nil {
		t.Error("a scheme-less base URL must be rejected")
	}
}

// feedAll pushes every event line through the translator and collects chunks.
func feedAll(t *testing.T, tr *cliTranslator, lines ...string) []string {
	t.Helper()
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		chunks, err := tr.feed([]byte(line))
		if err != nil {
			t.Fatalf("feed(%s): %v", line, err)
		}
		for _, chunk := range chunks {
			out = append(out, string(chunk))
		}
	}
	out = append(out, mapStrings(tr.finish())...)
	return out
}

func mapStrings(chunks [][]byte) []string {
	out := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		out = append(out, string(chunk))
	}
	return out
}

// assertChunkShapes verifies every emitted payload is a well-formed OpenAI
// streaming chunk.
func assertChunkShapes(t *testing.T, chunks []string) {
	t.Helper()
	if len(chunks) == 0 {
		t.Fatal("no chunks were emitted")
	}
	for _, chunk := range chunks {
		var probe struct {
			Object  string `json:"object"`
			Choices []struct {
				Index int `json:"index"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(chunk), &probe); err != nil {
			t.Fatalf("chunk is not JSON: %s", chunk)
		}
		if probe.Object != "chat.completion.chunk" {
			t.Errorf("chunk object = %q in %s", probe.Object, chunk)
		}
		if len(probe.Choices) != 1 {
			t.Errorf("chunk has %d choices: %s", len(probe.Choices), chunk)
		}
	}
}

func mustMap(t *testing.T, value any) map[string]any {
	t.Helper()
	out, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value is %T, want map[string]any", value)
	}
	return out
}

func anySlice(t *testing.T, value any) []any {
	t.Helper()
	out, ok := value.([]any)
	if !ok {
		t.Fatalf("value is %T, want []any", value)
	}
	return out
}
