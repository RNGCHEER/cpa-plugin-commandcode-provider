package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// HostHTTPDoer is the slice of the host bridge the catalog, quota page and
// executor need. It is an interface so tests can substitute a fake transport.
type HostHTTPDoer interface {
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
	DoStream(ctx context.Context, req pluginapi.HTTPRequest) (hostStream, error)
	Log(level, message string, fields map[string]any)
}

// rawCaller is the CGO bridge's outbound callback signature (implemented in
// package main over the C host API).
type rawCaller func(method string, payload []byte) ([]byte, error)

// hostBridge implements HostHTTPDoer over the host's RPC callbacks. All
// traffic flows through host.http.* so proxy policy, transport and request
// logging stay with the host.
type hostBridge struct {
	call rawCaller
}

func newHostBridge(call rawCaller) *hostBridge { return &hostBridge{call: call} }

// envelope mirrors the host's JSON response envelope.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error"`
}

func (b *hostBridge) do(method string, payload any) (json.RawMessage, error) {
	if b == nil || b.call == nil {
		return nil, fmt.Errorf("commandcode: host bridge unavailable")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("commandcode: encode %s: %w", method, err)
	}
	resp, err := b.call(method, raw)
	if err != nil {
		return nil, fmt.Errorf("commandcode: %s: %w", method, err)
	}
	var env envelope
	if err := json.Unmarshal(resp, &env); err != nil {
		return nil, fmt.Errorf("commandcode: decode %s envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, &hostRPCError{Code: env.Error.Code, Message: env.Error.Message, Status: env.Error.HTTPStatus}
		}
		return nil, fmt.Errorf("commandcode: %s failed", method)
	}
	return env.Result, nil
}

// hostRPCError carries the host's structured error (including http_status)
// back to the executor's retry classifier.
type hostRPCError struct {
	Code    string
	Message string
	Status  int
}

func (e *hostRPCError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// StatusCode reports the HTTP status the host surfaced, or 0.
func (e *hostRPCError) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.Status
}

// hostHTTPReq is the wire shape accepted by host.http.do / do_stream.
type hostHTTPReq struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers"`
	Body    []byte      `json:"body"`
}

// hostStream is one opened upstream stream: status, headers and the host's
// UPSTREAM stream id (readable via streamRead, closable via streamClose).
type hostStream struct {
	StatusCode int
	Headers    http.Header
	StreamID   string
}

// Do performs one non-streaming HTTP request through the host.
func (b *hostBridge) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if strings.TrimSpace(req.URL) == "" {
		return pluginapi.HTTPResponse{}, fmt.Errorf("commandcode: request URL is required")
	}
	if _, err := url.Parse(req.URL); err != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("commandcode: request URL is invalid")
	}
	result, err := b.do("host.http.do", hostHTTPReq{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body})
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	var resp pluginapi.HTTPResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("commandcode: decode host.http.do: %w", err)
	}
	return resp, nil
}

// hostStreamStartResp decodes the host.http.do_stream result.
type hostStreamStartResp struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}

// DoStream opens one streaming HTTP request through the host and returns the
// upstream stream handle. The caller must close it (streamClose) when done.
func (b *hostBridge) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (hostStream, error) {
	if strings.TrimSpace(req.URL) == "" {
		return hostStream{}, fmt.Errorf("commandcode: request URL is required")
	}
	result, err := b.do("host.http.do_stream", hostHTTPReq{Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body})
	if err != nil {
		return hostStream{}, err
	}
	var resp hostStreamStartResp
	if err := json.Unmarshal(result, &resp); err != nil {
		return hostStream{}, fmt.Errorf("commandcode: decode host.http.do_stream: %w", err)
	}
	if resp.StreamID == "" {
		return hostStream{}, fmt.Errorf("commandcode: host returned no stream id")
	}
	return hostStream{StatusCode: resp.StatusCode, Headers: resp.Headers, StreamID: resp.StreamID}, nil
}

// Log emits a line through host.log; fields are never allowed to carry keys.
func (b *hostBridge) Log(level, message string, fields map[string]any) {
	if b == nil || b.call == nil {
		return
	}
	payload := struct {
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Fields  map[string]any `json:"fields"`
	}{Level: level, Message: message, Fields: fields}
	if _, err := b.do("host.log", payload); err != nil {
		// Logging must never break the caller.
		_ = err
	}
}

// hostStreamIDReq addresses an existing stream by its id.
type hostStreamIDReq struct {
	StreamID string `json:"stream_id"`
}

// hostStreamReadResp decodes one host.http.stream_read result.
type hostStreamReadResp struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

// streamClose closes an upstream stream opened by DoStream.
func (b *hostBridge) streamClose(streamID string) error {
	if streamID == "" {
		return nil
	}
	_, err := b.do("host.http.stream_close", hostStreamIDReq{StreamID: streamID})
	return err
}

// streamRead reads one chunk from an upstream stream. The host blocks until
// the next chunk, a terminal error, or the stream end (done=true).
func (b *hostBridge) streamRead(streamID string) (hostStreamReadResp, error) {
	if streamID == "" {
		return hostStreamReadResp{}, fmt.Errorf("commandcode: stream id is required")
	}
	result, err := b.do("host.http.stream_read", hostStreamIDReq{StreamID: streamID})
	if err != nil {
		return hostStreamReadResp{}, err
	}
	var chunk hostStreamReadResp
	if err := json.Unmarshal(result, &chunk); err != nil {
		return hostStreamReadResp{}, fmt.Errorf("commandcode: decode stream_read: %w", err)
	}
	if chunk.Error != "" {
		return chunk, fmt.Errorf("%s", chunk.Error)
	}
	return chunk, nil
}

// isRetryableStatus reports whether an upstream status should fail over to the
// next key: rate limits, auth/quota exhaustion and transient upstream errors.
// 4xx request-shape errors (400/404/422) never fail over - they are the same
// request on the next key.
func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden,
		http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		http.StatusInternalServerError, http.StatusInsufficientStorage:
		return true
	}
	return status >= 520 && status <= 526
}

// isQuotaStatus reports whether the status signals this key's quota or plan is
// exhausted (the dsh plugin's "switch to the next account" trigger).
func isQuotaStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusPaymentRequired || status == http.StatusForbidden
}

// errorMessage extracts a human-readable message from an upstream error body.
func errorMessage(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var decoded struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
		if len(decoded.Error) > 0 {
			var obj struct {
				Message string `json:"message"`
			}
			if errObj := json.Unmarshal(decoded.Error, &obj); errObj == nil && strings.TrimSpace(obj.Message) != "" {
				return strings.TrimSpace(obj.Message)
			}
			var s string
			if errStr := json.Unmarshal(decoded.Error, &s); errStr == nil && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		if strings.TrimSpace(decoded.Message) != "" {
			return strings.TrimSpace(decoded.Message)
		}
	}
	if len(trimmed) > 500 {
		return trimmed[:500]
	}
	return trimmed
}
