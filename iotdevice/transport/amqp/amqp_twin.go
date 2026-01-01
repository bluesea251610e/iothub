package amqp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/Azure/go-amqp"
	"github.com/bluesea251610e/iothub/iotdevice/transport"
)

// twinRequestID is an atomic counter for twin request IDs
var twinRequestID uint32

// Twin AMQP addresses
const (
	twinGetAddress    = "$iothub/twin/GET"
	twinPatchAddress  = "$iothub/twin/PATCH/properties/reported"
	twinUpdateAddress = "$iothub/twin/PATCH/properties/desired/#"
)

// twinLinks holds sender/receiver links for twin operations
type twinLinks struct {
	sender   *amqp.Sender
	receiver *amqp.Receiver
	respChan map[uint32]chan *twinResponse
	mu       sync.RWMutex
}

type twinResponse struct {
	status  int
	version int
	body    []byte
}

// ensureTwinLinks creates twin links if not already created
func (tr *Transport) ensureTwinLinks(ctx context.Context) error {
	tr.twinMu.Lock()
	defer tr.twinMu.Unlock()

	if tr.twinSender != nil && tr.twinReceiver != nil {
		return nil
	}

	// Create sender for twin requests
	sender, err := tr.sess.NewSender(ctx, "$iothub/twin/request", nil)
	if err != nil {
		return fmt.Errorf("create twin sender: %w", err)
	}
	tr.twinSender = sender

	// Create receiver for twin responses
	receiver, err := tr.sess.NewReceiver(ctx, "$iothub/twin/response", nil)
	if err != nil {
		sender.Close(ctx)
		return fmt.Errorf("create twin receiver: %w", err)
	}
	tr.twinReceiver = receiver

	return nil
}

// RetrieveTwinProperties retrieves device twin properties via AMQP.
func (tr *Transport) RetrieveTwinPropertiesImpl(ctx context.Context) ([]byte, error) {
	if err := tr.ensureTwinLinks(ctx); err != nil {
		return nil, err
	}

	rid := atomic.AddUint32(&twinRequestID, 1)

	// Send GET request
	correlationID := fmt.Sprintf("%d", rid)
	msg := &amqp.Message{
		Properties: &amqp.MessageProperties{
			CorrelationID: &correlationID,
		},
		ApplicationProperties: map[string]any{
			"operation": "GET",
		},
	}

	if err := tr.twinSender.Send(ctx, msg, nil); err != nil {
		return nil, fmt.Errorf("send twin GET: %w", err)
	}

	// Wait for response
	respMsg, err := tr.twinReceiver.Receive(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("receive twin response: %w", err)
	}
	defer tr.twinReceiver.AcceptMessage(ctx, respMsg)

	// Check status
	status, ok := respMsg.ApplicationProperties["status"].(int32)
	if !ok {
		return nil, errors.New("missing status in twin response")
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("twin GET failed with status %d", status)
	}

	return respMsg.GetData(), nil
}

// UpdateTwinPropertiesImpl updates device twin reported properties via AMQP.
func (tr *Transport) UpdateTwinPropertiesImpl(ctx context.Context, payload []byte) (int, error) {
	if err := tr.ensureTwinLinks(ctx); err != nil {
		return 0, err
	}

	rid := atomic.AddUint32(&twinRequestID, 1)

	// Send PATCH request
	correlationID := fmt.Sprintf("%d", rid)
	msg := &amqp.Message{
		Data: [][]byte{payload},
		Properties: &amqp.MessageProperties{
			CorrelationID: &correlationID,
		},
		ApplicationProperties: map[string]any{
			"operation": "PATCH",
			"resource":  "/properties/reported",
		},
	}

	if err := tr.twinSender.Send(ctx, msg, nil); err != nil {
		return 0, fmt.Errorf("send twin PATCH: %w", err)
	}

	// Wait for response
	respMsg, err := tr.twinReceiver.Receive(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("receive twin response: %w", err)
	}
	defer tr.twinReceiver.AcceptMessage(ctx, respMsg)

	// Check status
	status, ok := respMsg.ApplicationProperties["status"].(int32)
	if !ok {
		return 0, errors.New("missing status in twin response")
	}
	if status < 200 || status >= 300 {
		return 0, fmt.Errorf("twin PATCH failed with status %d", status)
	}

	// Get version from response
	version := 0
	if v, ok := respMsg.ApplicationProperties["$version"].(int32); ok {
		version = int(v)
	}

	return version, nil
}

// SubscribeTwinUpdatesImpl subscribes to twin desired property updates via AMQP.
func (tr *Transport) SubscribeTwinUpdatesImpl(ctx context.Context, mux transport.TwinStateDispatcher) error {
	// Create receiver for twin updates
	addr := "$iothub/twin/PATCH/properties/desired"
	receiver, err := tr.sess.NewReceiver(ctx, addr, nil)
	if err != nil {
		return fmt.Errorf("create twin update receiver: %w", err)
	}

	// Start receiving updates in background
	go tr.receiveTwinUpdates(ctx, receiver, mux)

	return nil
}

func (tr *Transport) receiveTwinUpdates(ctx context.Context, receiver *amqp.Receiver, mux transport.TwinStateDispatcher) {
	defer receiver.Close(context.Background())

	for {
		select {
		case <-tr.done:
			return
		case <-ctx.Done():
			return
		default:
		}

		msg, err := receiver.Receive(ctx, nil)
		if err != nil {
			tr.logError("twin update receive error: %s", err)
			return
		}

		// Dispatch twin update
		mux.Dispatch(msg.GetData())

		if err := receiver.AcceptMessage(ctx, msg); err != nil {
			tr.logError("accept twin update error: %s", err)
		}
	}
}

// TwinState represents twin state.
type TwinState struct {
	Desired  map[string]interface{} `json:"desired"`
	Reported map[string]interface{} `json:"reported"`
}

// parseTwinState parses twin JSON response.
func parseTwinState(data []byte) (*TwinState, error) {
	var state TwinState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}
