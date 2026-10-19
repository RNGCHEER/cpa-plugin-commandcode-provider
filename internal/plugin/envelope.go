package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
)

// envelope is the host's JSON response envelope.
type rpcEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
}

// statusCoder is implemented by this package's error types so the envelope
// can carry an HTTP status to the host.
type statusCoder interface{ StatusCode() int }

// okEnvelope wraps a result in the success envelope.
func okEnvelope(result any) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rpcEnvelope{OK: true, Result: raw})
}

// OkEnvelope renders a success envelope for callers outside this package
// (the dispatcher returns (byte, error) from handlers). It mirrors
// okEnvelope so both paths produce the same wire shape.
func OkEnvelope(result any) ([]byte, error) {
	return okEnvelope(result)
}

// ErrEnvelope renders a failure envelope for callers outside this package
// (the CGO main glue uses it for boundary errors). It mirrors
// errorEnvelope so both paths produce the same wire shape.
func ErrEnvelope(code, message string) []byte {
	return errorEnvelope(code, message)
}

// errorEnvelope wraps a failure in the error envelope (no HTTP status).
func errorEnvelope(code, message string) []byte {
	return errorEnvelopeWithStatus(code, message, 0)
}

// errorEnvelopeWithStatus carries an HTTP status so the host can surface it
// to the client and classify retries.
func errorEnvelopeWithStatus(code, message string, status int) []byte {
	raw, _ := json.Marshal(rpcEnvelope{
		OK: false,
		Error: &rpcError{
			Code:       code,
			Message:    message,
			HTTPStatus: status,
			Retryable:  isRetryableStatus(status),
		},
	})
	return raw
}

// errorFrom converts an execution error into an error envelope, preserving
// any HTTP status the error carries so the host retries the right way.
func errorFrom(err error) []byte {
	if err == nil {
		return errorEnvelope("internal_error", "unknown failure")
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		if status := sc.StatusCode(); status != 0 {
			return errorEnvelopeWithStatus(classify(err), err.Error(), status)
		}
	}
	if rpcErr, ok := err.(*hostRPCError); ok && rpcErr != nil {
		return errorEnvelopeWithStatus(rpcErr.Code, rpcErr.Message, rpcErr.Status)
	}
	return errorEnvelope(classify(err), err.Error())
}

// classify maps an error to a stable envelope code for the host's logs.
func classify(err error) string {
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		switch status := sc.StatusCode(); status {
		case http.StatusUnauthorized:
			return "auth_failed"
		case http.StatusPaymentRequired, http.StatusForbidden:
			return "quota_exceeded"
		case http.StatusTooManyRequests:
			return "rate_limited"
		case http.StatusNotFound:
			return "model_not_found"
		}
	}
	return "upstream_error"
}
