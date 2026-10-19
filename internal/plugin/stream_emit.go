package plugin

import (
	"context"
	"fmt"
	"time"
)

// streamEmit writes one normalized payload to the DOWNSTREAM stream (the
// host-allocated id from executor.execute_stream). Bounded so a wedged
// downstream consumer surfaces as an error instead of a parked goroutine.
func (b *hostBridge) streamEmit(downstreamStreamID string, payload []byte) error {
	if downstreamStreamID == "" {
		return fmt.Errorf("commandcode: downstream stream id is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ctx
	_, err := b.do("host.stream.emit", struct {
		StreamID string `json:"stream_id"`
		Payload  []byte `json:"payload"`
	}{StreamID: downstreamStreamID, Payload: payload})
	return err
}

// streamCloseDownstream terminates the DOWNSTREAM stream, optionally with an
// error label the client should see.
func (b *hostBridge) streamCloseDownstream(downstreamStreamID, errMsg string) error {
	if downstreamStreamID == "" {
		return nil
	}
	_, err := b.do("host.stream.close", struct {
		StreamID string `json:"stream_id"`
		Error    string `json:"error,omitempty"`
	}{StreamID: downstreamStreamID, Error: errMsg})
	return err
}
