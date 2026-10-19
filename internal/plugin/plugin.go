package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// authKeyHash is the non-secret digest identifying one configured api-key. It
// is independent of config ordering, so the cards derived from it stay stable
// across restarts, and it never exposes the key itself to the page or logs.
func authKeyHash(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

// Manager owns the plugin's dispatcher state (config snapshot, catalog,
// background refresh) and routes every host RPC method. It is safe for
// concurrent HandleCall use.
type Manager struct {
	bridge  *hostBridge
	catalog *catalogState

	mu  sync.RWMutex
	cfg config

	lifeMu sync.Mutex
	stop   chan struct{}
	done   chan struct{}
}

// NewManager returns a dispatcher whose outbound traffic flows through the
// injected raw host caller.
func NewManager(call rawCaller) *Manager {
	return &Manager{
		bridge:  newHostBridge(call),
		catalog: &catalogState{},
		cfg:     defaultConfig(),
	}
}

// snapshot returns the current config and the host HTTP client.
func (m *Manager) snapshot() (config, HostHTTPDoer) {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	return cfg, m.bridge
}

// HandleCall dispatches one RPC method. Handler failures travel inside the
// envelope; a recovered panic becomes a plugin_error envelope so the host
// process never dies with us.
func (m *Manager) HandleCall(method string, request []byte) (resp []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			m.bridge.Log("error", "commandcode: handler panic contained", map[string]any{"method": method})
			resp = errorEnvelope("plugin_error", fmt.Sprintf("internal error handling %s", method))
			err = nil
		}
	}()
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return m.handleLifecycle(request)
	case "plugin.shutdown":
		return m.handleShutdown()
	case "plugin.quiesce":
		return okEnvelope(struct{}{})
	case "model.static", "model.for_auth":
		return m.handleModels(request)
	case "executor.identifier":
		return okEnvelope(map[string]string{"identifier": ProviderID})
	case "executor.execute":
		return m.handleExecute(request)
	case "executor.execute_stream":
		return m.handleExecuteStream(request)
	case "executor.count_tokens":
		return m.handleCountTokens(request)
	case "executor.http_request":
		return errorEnvelope("unsupported", "commandcode does not expose a raw HTTP surface"), nil
	case "auth.identifier":
		return okEnvelope(map[string]string{"identifier": ProviderID})
	case "auth.parse":
		var req pluginapi.AuthParseRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "malformed auth parse request"), nil
		}
		result, errParse := parseAuth(req)
		if errParse != nil {
			return errorEnvelope("auth_failure", errParse.Error()), nil
		}
		return OkEnvelope(result)
	case "auth.refresh":
		var req pluginapi.AuthRefreshRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "malformed auth refresh request"), nil
		}
		result, errRefresh := refreshAuth(req)
		if errRefresh != nil {
			return errorEnvelope("auth_failure", errRefresh.Error()), nil
		}
		return OkEnvelope(result)
	case "auth.login.start":
		var req struct {
			pluginapi.AuthLoginStartRequest
		}
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "malformed login start request"), nil
		}
		if _, errLogin := startLogin(req.AuthLoginStartRequest); errLogin != nil {
			return errorEnvelope("unsupported", errLogin.Error()), nil
		}
		return errorEnvelope("unsupported", "commandcode sign-in is not ported"), nil
	case "auth.login.poll":
		var req struct{ pluginapi.AuthLoginPollRequest }
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "malformed login poll request"), nil
		}
		result, errPoll := pollLogin(req.AuthLoginPollRequest)
		if errPoll != nil {
			return errorEnvelope("unsupported", errPoll.Error()), nil
		}
		return OkEnvelope(result)
	case "management.register":
		return managementRegistration()
	case "management.handle":
		var req struct {
			pluginapi.ManagementRequest
			HostCallbackID string `json:"host_callback_id,omitempty"`
		}
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "malformed management request"), nil
		}
		result, errMgmt := m.handleManagement(req.ManagementRequest)
		if errMgmt != nil {
			return errorEnvelope("management_failure", errMgmt.Error()), nil
		}
		return OkEnvelope(result)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// lifecycleRequest mirrors the host's lifecycle RPC: config_yaml is raw YAML
// for this plugin (enabled/priority are consumed by the host first).
type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

// handleLifecycle loads the config, warms the catalog once and starts the
// background refresh loop. Registration succeeds even when the initial
// catalog fetch fails: an empty model list is a valid "not ready" state and
// the loop retries.
func (m *Manager) handleLifecycle(request []byte) ([]byte, error) {
	var req lifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return errorEnvelope("invalid_request", "malformed lifecycle request"), nil
		}
	}
	cfg, err := loadConfig(req.ConfigYAML)
	if err != nil {
		return errorEnvelope("invalid_config", err.Error()), nil
	}
	m.lifeMu.Lock()
	defer m.lifeMu.Unlock()

	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()

	// Warm the catalog synchronously with a short bound so host startup is
	// never held hostage by a slow upstream.
	warmCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if _, errWarm := m.catalog.fetchCatalog(warmCtx, cfg, m.bridge); errWarm != nil {
		m.bridge.Log("warn", "commandcode: initial catalog fetch failed (will retry in background)", map[string]any{"reason": errWarm.Error()})
	}
	cancel()

	m.startRefreshLoopLocked(cfg)
	return registrationEnvelope()
}

// startRefreshLoopLocked restarts the background catalog refresh loop. The
// caller holds lifeMu.
func (m *Manager) startRefreshLoopLocked(cfg config) {
	m.stopRefreshLoopLocked()
	stop := make(chan struct{})
	done := make(chan struct{})
	m.stop, m.done = stop, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, err := m.catalog.fetchCatalog(context.Background(), cfg, m.bridge); err != nil {
					m.bridge.Log("warn", "commandcode: catalog refresh failed (serving last valid catalog)", map[string]any{"reason": err.Error()})
				}
			}
		}
	}()
}

// stopRefreshLoopLocked stops the running loop and waits for it. The caller
// holds lifeMu.
func (m *Manager) stopRefreshLoopLocked() {
	if m.stop == nil {
		return
	}
	close(m.stop)
	<-m.done
	m.stop, m.done = nil, nil
}

// handleShutdown stops background work. The host calls this before it frees
// the plugin, so no goroutine may outlive it.
func (m *Manager) handleShutdown() ([]byte, error) {
	m.lifeMu.Lock()
	m.stopRefreshLoopLocked()
	m.lifeMu.Unlock()
	return okEnvelope(struct{}{})
}

// handleExecute serves executor.execute, including stream-flagged requests
// (the host may hand either shape to either method).
func (m *Manager) handleExecute(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", "malformed executor request"), nil
	}
	if req.Stream {
		return m.handleExecuteStream(request)
	}
	cfg, url, body, errResp := m.prepare(req)
	if errResp != nil {
		return errResp, nil
	}
	result, errExec := m.execute(context.Background(), cfg, req, body, url)
	if errExec != nil {
		return errorFrom(errExec), nil
	}
	return okEnvelope(result)
}

// handleExecuteStream starts the streaming pump: pre-first-byte failures
// return as error envelopes so the host can fail over; on success the pump
// runs in the background and the envelope returns immediately so the host can
// drain the downstream stream.
func (m *Manager) handleExecuteStream(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", "malformed executor stream request"), nil
	}
	if strings.TrimSpace(req.StreamID) == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream"), nil
	}
	cfg, url, body, errResp := m.prepare(req)
	if errResp != nil {
		return errResp, nil
	}
	status, headers, upstreamID, errStream := m.executeStream(context.Background(), cfg, req, body, url)
	if errStream != nil {
		if status >= 400 {
			return errorFrom(errStream), nil
		}
		return errorFrom(errStream), nil
	}
	_ = status
	_ = headers
	go m.pumpStream(cfg, req.StreamID, upstreamID, req.SourceFormat)
	return okEnvelope(struct {
		Headers http.Header `json:"headers"`
	}{Headers: http.Header{"Content-Type": []string{"text/event-stream"}}})
}

// prepare resolves the model, builds the upstream URL and rewrites the client
// payload for the upstream transport. A non-nil []byte is a ready-made error
// envelope.
func (m *Manager) prepare(req executorRequest) (config, string, []byte, []byte) {
	cfg := m.snapshotConfig()
	catalogID, ok := cfg.catalogModelID(req.Model)
	if !ok {
		return cfg, "", nil, errorEnvelope("model_not_found", "model "+req.Model+" is not a commandcode model")
	}
	// Translate the client payload (openai / claude / openai-response) to the
	// one upstream transport (chat-completions), then force the stream flag.
	translated, errTranslate := TranslateRequest(req.SourceFormat, req.OriginalRequest, catalogID, req.Stream)
	if errTranslate != nil {
		return cfg, "", nil, errorEnvelope("translation_failed", errTranslate.Error())
	}
	url := strings.TrimSuffix(cfg.BaseURL, "/") + "/chat/completions"
	return cfg, url, translated, nil
}

func (m *Manager) snapshotConfig() config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// pumpStream reads the upstream SSE stream, normalizes each line (reasoning
// backfill + route framing) and emits it downstream until the stream ends.
func (m *Manager) pumpStream(cfg config, downstreamID, upstreamID, sourceFormat string) {
	var closeOnce sync.Once
	closeStreams := func(errMsg string) {
		closeOnce.Do(func() {
			_ = m.bridge.streamClose(upstreamID)
			_ = m.bridge.streamCloseDownstream(downstreamID, errMsg)
		})
	}
	defer closeStreams("")

	var pending []byte
	for {
		chunk, errRead := m.bridge.streamRead(upstreamID)
		if errRead != nil {
			closeStreams(redact(errRead.Error()))
			return
		}
		if chunk.Error != "" {
			closeStreams(redact(chunk.Error))
			return
		}
		pending = append(pending, chunk.Payload...)
		lines, rest := parseStreamLines(pending)
		pending = rest
		for _, line := range lines {
			payload := backfillChunk(line.payload)
			framed := frameForRoute(sourceFormat, payload)
			if errEmit := m.bridge.streamEmit(downstreamID, framed); errEmit != nil {
				closeStreams(redact(errEmit.Error()))
				return
			}
		}
		if chunk.Done {
			// Flush any unterminated tail line the upstream closed on.
			if lines, _ := parseStreamLines(append(pending, '\n')); len(lines) > 0 {
				for _, line := range lines {
					payload := backfillChunk(line.payload)
					if errEmit := m.bridge.streamEmit(downstreamID, frameForRoute(sourceFormat, payload)); errEmit != nil {
						closeStreams(redact(errEmit.Error()))
						return
					}
				}
			}
			return
		}
	}
}

// frameForRoute renders one normalized payload in the framing the downstream
// route expects: Claude /v1/messages consumes SSE-framed input (one data:
// line), OpenAI chat and Responses routes consume bare JSON payloads.
func frameForRoute(sourceFormat string, payload []byte) []byte {
	if sourceFormat == "claude" {
		out := make([]byte, 0, len(payload)+6)
		out = append(out, []byte("data: ")...)
		out = append(out, payload...)
		return out
	}
	return payload
}

// handleCountTokens answers with a local estimate: the upstream exposes no
// tokenize endpoint.
func (m *Manager) handleCountTokens(request []byte) ([]byte, error) {
	var req executorRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return errorEnvelope("invalid_request", "malformed count_tokens request"), nil
	}
	count := int64(len(req.Payload) / 4)
	if count < 1 && len(req.Payload) > 0 {
		count = 1
	}
	payload, _ := json.Marshal(map[string]any{
		"prompt_tokens":     count,
		"completion_tokens": 0,
		"total_tokens":      count,
	})
	return okEnvelope(pluginapi.ExecutorResponse{Payload: payload})
}

// ---- host helpers used by the quota page and lifecycle -----------------------

// hostAuthList lists the host's credential records (for the quota page).
func (m *Manager) hostAuthList(_ context.Context) ([]pluginapi.HostAuthFileEntry, error) {
	result, err := m.bridge.do("host.auth.list", struct{}{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", err)
	}
	return resp.Files, nil
}

// hostAuthKey reads one credential's JSON through the host and returns its
// api key. The value never leaves this function's caller scope.
func (m *Manager) hostAuthKey(_ context.Context, authIndex string) (string, error) {
	result, err := m.bridge.do("host.auth.get", map[string]string{"auth_index": authIndex})
	if err != nil {
		return "", err
	}
	var resp struct {
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return "", fmt.Errorf("decode host.auth.get: %w", err)
	}
	var storage authStorage
	if err := json.Unmarshal(resp.JSON, &storage); err == nil && strings.TrimSpace(storage.APIKey) != "" {
		return strings.TrimSpace(storage.APIKey), nil
	}
	var official struct {
		APIKey string `json:"apiKey"`
		Token  string `json:"token"`
	}
	if err := json.Unmarshal(resp.JSON, &official); err == nil {
		if key := strings.TrimSpace(official.APIKey); key != "" {
			return key, nil
		}
		return strings.TrimSpace(official.Token), nil
	}
	return "", fmt.Errorf("credential carries no api key")
}

// redact trims an error message for the downstream stream close so it never
// carries upstream bodies.
func redact(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
