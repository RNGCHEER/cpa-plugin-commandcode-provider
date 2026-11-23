package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// executorRequest mirrors the host's executor RPC: pluginapi.ExecutorRequest is
// embedded untagged, so its fields marshal under Go names; StreamID is the
// DOWNSTREAM (host-allocated) stream for executor.execute_stream emissions.
type executorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// keyAttempt is one key's place in the failover order.
type keyAttempt struct {
	// key is the credential presented to the upstream. Empty means the auth
	// record did not carry one (router path with nil auth).
	key string
	// fromAuth marks the selected auth record, which is always tried first.
	fromAuth bool
}

// attemptOrder builds the failover order for one request: the selected auth
// record first, then every configured pool key that is not already the auth
// key. This is the port of the dsh plugin's "switch to the next account when
// one hits its usage limit" behaviour, expressed through the host's own
// credential selection plus this pool.
func attemptOrder(cfg config, req executorRequest) []keyAttempt {
	seen := map[string]bool{}
	var out []keyAttempt
	if authKey := strings.TrimSpace(req.AuthAttributes["api_key"]); authKey != "" && !seen[authKey] {
		seen[authKey] = true
		out = append(out, keyAttempt{key: authKey, fromAuth: true})
	} else if authKey := authKeyFromStorage(req.StorageJSON); authKey != "" && !seen[authKey] {
		seen[authKey] = true
		out = append(out, keyAttempt{key: authKey, fromAuth: true})
	}
	for _, key := range cfg.APIKeys {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, keyAttempt{key: key})
	}
	return out
}

// authKeyFromStorage is the fallback when the host handed us a storage blob
// without the attribute mirror (router/static path).
func authKeyFromStorage(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var storage authStorage
	if err := json.Unmarshal(raw, &storage); err != nil {
		return ""
	}
	return strings.TrimSpace(storage.APIKey)
}

// upstreamError carries an upstream HTTP status back to the host so its retry
// classification (and the next credential) can engage.
type upstreamError struct {
	status int
	msg    string
	body   []byte
}

func (e *upstreamError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.msg) != "" {
		return e.msg
	}
	if len(e.body) > 0 {
		return errorMessage(e.body)
	}
	return fmt.Sprintf("commandcode: upstream status %d", e.status)
}

// StatusCode reports the upstream status to the host's envelope encoder.
func (e *upstreamError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.status
}

// catalogIDFor resolves a client-visible model id back to the upstream catalog
// id, falling back to the input when the host handed us an unprefixed id.
func (m *Manager) catalogIDFor(clientModelID string) string {
	cfg := m.snapshotConfig()
	if id, ok := cfg.catalogModelID(clientModelID); ok {
		return id
	}
	return clientModelID
}

// execute uses only the documented Provider API. Entitlement denials are final.
func (m *Manager) execute(ctx context.Context, cfg config, req executorRequest, body []byte, url string) (pluginapi.ExecutorResponse, error) {
	attempts := attemptOrder(cfg, req)
	var last error = &upstreamError{status: 401, msg: "commandcode: no api key available"}
	for _, attempt := range attempts {
		if ctx.Err() != nil {
			return pluginapi.ExecutorResponse{}, ctx.Err()
		}
		callCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		resp, err := m.bridge.Do(callCtx, pluginapi.HTTPRequest{Method: http.MethodPost, URL: url, Headers: providerHeaders(cfg, attempt.key, false, url), Body: body})
		cancel()
		if err != nil {
			last = err
			if retryableTransport(err) {
				continue
			}
			return pluginapi.ExecutorResponse{}, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			fixed, _ := mapReasoningBody(resp.Body)
			return pluginapi.ExecutorResponse{Payload: fixed, Headers: resp.Headers}, nil
		}
		last = providerError(resp.StatusCode, resp.Body)
		if isUpgradeRequired(resp.StatusCode, resp.Body) || !isRetryableStatus(resp.StatusCode) {
			return pluginapi.ExecutorResponse{}, last
		}
	}
	return pluginapi.ExecutorResponse{}, last
}

func (m *Manager) executeStream(ctx context.Context, cfg config, req executorRequest, body []byte, url string) (int, http.Header, string, bool, error) {
	var last error = &upstreamError{status: 401, msg: "commandcode: no api key available"}
	for _, attempt := range attemptOrder(cfg, req) {
		if ctx.Err() != nil {
			return 0, nil, "", false, ctx.Err()
		}
		callCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		resp, err := m.bridge.DoStream(callCtx, pluginapi.HTTPRequest{Method: http.MethodPost, URL: url, Headers: providerHeaders(cfg, attempt.key, true, url), Body: body})
		cancel()
		if err != nil {
			last = err
			if retryableTransport(err) {
				continue
			}
			return 0, nil, "", false, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.StatusCode, resp.Headers, resp.StreamID, false, nil
		}
		body := drainStreamError(m.bridge, resp.StreamID)
		_ = m.bridge.streamClose(resp.StreamID)
		last = providerError(resp.StatusCode, body)
		if isUpgradeRequired(resp.StatusCode, body) || !isRetryableStatus(resp.StatusCode) {
			return resp.StatusCode, resp.Headers, "", false, last
		}
	}
	return 0, nil, "", false, last
}

func providerError(status int, body []byte) error {
	if isUpgradeRequired(status, body) {
		return &upstreamError{status: status, msg: "commandcode: Provider API access is required (403 upgrade_required); use a plan with documented API access"}
	}
	return &upstreamError{status: status, body: body}
}

// logFailover records why a key was skipped. Fields never carry the key.
func (m *Manager) logFailover(cfg config, attempt keyAttempt, err error) {
	if m.bridge == nil {
		return
	}
	fields := map[string]any{
		"from_auth": attempt.fromAuth,
	}
	if err != nil {
		fields["reason"] = err.Error()
	}
	m.bridge.Log("warn", "commandcode: api unavailable, trying next key", fields)
}

// retryableTransport reports whether a host-side transport error should move
// to the next key. Host RPC errors that carry a retryable status do; plain
// network errors do.
func retryableTransport(err error) bool {
	if err == nil {
		return false
	}
	if rpcErr, ok := err.(*hostRPCError); ok {
		if rpcErr.Status != 0 {
			return isRetryableStatus(rpcErr.Status)
		}
		return true
	}
	// A status-carrying upstream error reached us through the host envelope.
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil && sc.StatusCode() != 0 {
		return isRetryableStatus(sc.StatusCode())
	}
	return true
}

// withAttemptTimeout bounds one upstream attempt by the configured request
// timeout. The host's own HTTP client governs transport teardown; this only
// guarantees one stalled key cannot eat the whole request budget.
func withAttemptTimeout(ctx context.Context, cfg config) context.Context {
	if cfg.RequestTimeout <= 0 {
		return ctx
	}
	attemptCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	// The cancel is owned by the host call below (Do/DoStream run to
	// completion inside this call), so it is safe to drop here only when the
	// call never outlives it. Both call sites run synchronously in this frame.
	_ = cancel
	return attemptCtx
}

// upstreamHeaders builds the per-key upstream headers (bearer auth, SSE
// accept when streaming).
func upstreamHeaders(key string, stream bool) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "cliproxyapi-commandcode-provider")
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

// drainStreamError reads at most one chunk of a failed stream's body for the
// error message. The caller closes the stream.
func drainStreamError(bridge *hostBridge, streamID string) []byte {
	if bridge == nil || streamID == "" {
		return nil
	}
	chunk, err := bridge.streamRead(streamID)
	if err != nil {
		return nil
	}
	return chunk.Payload
}
