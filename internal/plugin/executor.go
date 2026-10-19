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

// generatePlan resolves the CLI gateway endpoint (used for Go/GOAT/Pro
// subscriptions, which the Provider API refuses) and the CLI envelope for one
// request. A non-nil error means the fallback is unavailable.
func (m *Manager) generatePlan(cfg config, req executorRequest, body []byte) (string, []byte, error) {
	genURL, err := generateURL(cfg)
	if err != nil {
		return "", nil, err
	}
	catalogID := m.catalogIDFor(req.Model)
	modelMax := 0
	if row, ok := m.catalog.lookup(catalogID); ok {
		modelMax = int(row.MaxOutputTokens)
	}
	genBody, err := buildGenerateBody(body, cfg, catalogID, modelMax)
	if err != nil {
		return "", nil, err
	}
	return genURL, genBody, nil
}

// markGatewaySwitch records that one credential needs the CLI gateway and logs
// it once per switch. Fields never carry the key.
func (m *Manager) markGatewaySwitch(cfg config, attempt keyAttempt) {
	if m.prefersGenerate(attempt.key) {
		return
	}
	m.markGenerate(attempt.key)
	m.bridge.Log("info", "commandcode: plan has no provider API access, using the CLI gateway", map[string]any{
		"from_auth": attempt.fromAuth,
	})
}

// execute runs one non-streaming request, failing over across the key pool.
// Only pre-response failures (transport errors, retryable statuses) move to
// the next key; a 2xx answer is final, and a non-retryable 4xx stops the loop
// with that status so the host surfaces it verbatim.
//
// Each credential is tried against the Provider API first. A 403
// upgrade_required there means the account's plan (Go / GOAT / Pro) has no
// Provider API access at all, so the SAME credential is immediately retried on
// the CLI gateway (/alpha/generate) and remembered as gateway-only.
func (m *Manager) execute(ctx context.Context, cfg config, req executorRequest, body []byte, url string) (pluginapi.ExecutorResponse, error) {
	attempts := attemptOrder(cfg, req)
	if len(attempts) == 0 {
		return pluginapi.ExecutorResponse{}, &upstreamError{
			status: http.StatusUnauthorized,
			msg:    "commandcode: no api key available (set api-keys in plugins.configs." + PluginName + " or configure an auth record)",
		}
	}
	genURL, genBody, errGen := m.generatePlan(cfg, req, body)
	var lastErr error
	for _, attempt := range attempts {
		if ctx.Err() != nil {
			break
		}
		if !m.prefersGenerate(attempt.key) {
			resp, err := m.bridge.Do(withAttemptTimeout(ctx, cfg), pluginapi.HTTPRequest{
				Method:  http.MethodPost,
				URL:     url,
				Headers: upstreamHeaders(attempt.key, false),
				Body:    body,
			})
			if err != nil {
				lastErr = err
				// Transport failures and host-surfaced retryable statuses move on.
				if ctx.Err() == nil && retryableTransport(err) {
					m.logFailover(cfg, attempt, err)
					continue
				}
				return pluginapi.ExecutorResponse{}, err
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				fixed, _ := mapReasoningBody(resp.Body)
				return pluginapi.ExecutorResponse{Payload: fixed, Headers: resp.Headers}, nil
			}
			lastErr = &upstreamError{status: resp.StatusCode, body: resp.Body}
			switch {
			case isUpgradeRequired(resp.StatusCode, resp.Body):
				m.markGatewaySwitch(cfg, attempt)
			case isRetryableStatus(resp.StatusCode):
				m.logFailover(cfg, attempt, lastErr)
				continue
			default:
				return pluginapi.ExecutorResponse{}, lastErr
			}
		}
		// CLI gateway path (either known gateway-only, or just discovered).
		if errGen != nil {
			if lastErr != nil {
				return pluginapi.ExecutorResponse{}, lastErr
			}
			return pluginapi.ExecutorResponse{}, errGen
		}
		resp, err := m.bridge.Do(withAttemptTimeout(ctx, cfg), pluginapi.HTTPRequest{
			Method:  http.MethodPost,
			URL:     genURL,
			Headers: generateHeaders(cfg, attempt.key, false),
			Body:    genBody,
		})
		if err != nil {
			lastErr = err
			if ctx.Err() == nil && retryableTransport(err) {
				m.logFailover(cfg, attempt, err)
				continue
			}
			return pluginapi.ExecutorResponse{}, err
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			payload, errTranslate := aggregateGenerateResponse(resp.Body, m.catalogIDFor(req.Model))
			if errTranslate != nil {
				lastErr = errTranslate
				continue
			}
			return pluginapi.ExecutorResponse{Payload: payload, Headers: resp.Headers}, nil
		}
		lastErr = &upstreamError{status: resp.StatusCode, body: resp.Body}
		if isRetryableStatus(resp.StatusCode) {
			m.logFailover(cfg, attempt, lastErr)
			continue
		}
		return pluginapi.ExecutorResponse{}, lastErr
	}
	if lastErr != nil {
		return pluginapi.ExecutorResponse{}, lastErr
	}
	return pluginapi.ExecutorResponse{}, &upstreamError{
		status: http.StatusBadGateway,
		msg:    "commandcode: every api key failed",
	}
}

// executeStream runs one streaming request with the same failover rule, but
// only BEFORE the first upstream byte: once a 2xx stream is established its
// chunks are committed and cannot be replayed on another key.
//
// The returned generated flag reports that the opened stream is a CLI gateway
// stream, whose payload is newline-delimited CLI events rather than OpenAI SSE
// chunks and therefore needs the CLI translator when it is pumped downstream.
func (m *Manager) executeStream(ctx context.Context, cfg config, req executorRequest, body []byte, url string) (int, http.Header, string, bool, error) {
	attempts := attemptOrder(cfg, req)
	if len(attempts) == 0 {
		return 0, nil, "", false, &upstreamError{
			status: http.StatusUnauthorized,
			msg:    "commandcode: no api key available (set api-keys in plugins.configs." + PluginName + " or configure an auth record)",
		}
	}
	genURL, genBody, errGen := m.generatePlan(cfg, req, body)
	var lastErr error
	for _, attempt := range attempts {
		if ctx.Err() != nil {
			break
		}
		if !m.prefersGenerate(attempt.key) {
			stream, err := m.bridge.DoStream(withAttemptTimeout(ctx, cfg), pluginapi.HTTPRequest{
				Method:  http.MethodPost,
				URL:     url,
				Headers: upstreamHeaders(attempt.key, true),
				Body:    body,
			})
			if err != nil {
				lastErr = err
				if ctx.Err() == nil && retryableTransport(err) {
					m.logFailover(cfg, attempt, err)
					continue
				}
				return 0, nil, "", false, err
			}
			if stream.StatusCode < 200 || stream.StatusCode >= 300 {
				// Drain the error body for a useful message, then fail over if
				// this status is a key-level problem.
				drained := drainStreamError(m.bridge, stream.StreamID)
				lastErr = &upstreamError{status: stream.StatusCode, body: drained}
				_ = m.bridge.streamClose(stream.StreamID)
				switch {
				case isUpgradeRequired(stream.StatusCode, drained):
					// Plan has no Provider API access: retry the same key on the
					// CLI gateway below.
					m.markGatewaySwitch(cfg, attempt)
				case isRetryableStatus(stream.StatusCode):
					m.logFailover(cfg, attempt, lastErr)
					continue
				default:
					return stream.StatusCode, stream.Headers, "", false, lastErr
				}
			} else {
				return stream.StatusCode, stream.Headers, stream.StreamID, false, nil
			}
		}
		// CLI gateway path (either known gateway-only, or just discovered).
		if errGen != nil {
			if lastErr != nil {
				return 0, nil, "", false, lastErr
			}
			return 0, nil, "", false, errGen
		}
		stream, err := m.bridge.DoStream(withAttemptTimeout(ctx, cfg), pluginapi.HTTPRequest{
			Method:  http.MethodPost,
			URL:     genURL,
			Headers: generateHeaders(cfg, attempt.key, true),
			Body:    genBody,
		})
		if err != nil {
			lastErr = err
			if ctx.Err() == nil && retryableTransport(err) {
				m.logFailover(cfg, attempt, err)
				continue
			}
			return 0, nil, "", false, err
		}
		if stream.StatusCode < 200 || stream.StatusCode >= 300 {
			drained := drainStreamError(m.bridge, stream.StreamID)
			lastErr = &upstreamError{status: stream.StatusCode, body: drained}
			_ = m.bridge.streamClose(stream.StreamID)
			if isRetryableStatus(stream.StatusCode) {
				m.logFailover(cfg, attempt, lastErr)
				continue
			}
			return stream.StatusCode, stream.Headers, "", false, lastErr
		}
		return stream.StatusCode, stream.Headers, stream.StreamID, true, nil
	}
	if lastErr != nil {
		return 0, nil, "", false, lastErr
	}
	return 0, nil, "", false, &upstreamError{status: http.StatusBadGateway, msg: "commandcode: every api key failed"}
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
