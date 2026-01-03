package amqp

import (
	"context"
	"time"

	"github.com/Azure/go-amqp"
)

const (
	// tokenUpdateInterval is how long tokens are valid
	tokenUpdateInterval = time.Hour

	// tokenUpdateSpan is how long before expiry we refresh
	tokenUpdateSpan = 10 * time.Minute
)

// startCBSAuth starts CBS (Claims-Based Security) authentication.
// It authenticates once and then maintains token refresh in background.
func (tr *Transport) startCBSAuth(ctx context.Context) error {
	// Create a dedicated session for CBS
	cbsSess, err := tr.conn.NewSession(ctx, nil)
	if err != nil {
		return err
	}
	tr.cbsSess = cbsSess

	// Perform initial token put
	if err := tr.putToken(ctx, cbsSess, tokenUpdateInterval); err != nil {
		cbsSess.Close(context.Background())
		return err
	}

	// Start background token refresh
	go tr.tokenRefreshLoop(cbsSess)

	return nil
}

// putToken sends a CBS token to authenticate with IoT Hub.
func (tr *Transport) putToken(ctx context.Context, sess *amqp.Session, lifetime time.Duration) error {
	// Create sender to $cbs
	sender, err := sess.NewSender(ctx, "$cbs", nil)
	if err != nil {
		return err
	}
	defer sender.Close(context.Background())

	// Create receiver from $cbs
	receiver, err := sess.NewReceiver(ctx, "$cbs", nil)
	if err != nil {
		return err
	}
	defer receiver.Close(context.Background())

	// Generate SAS token
	resource := tr.creds.GetHostName() + "/devices/" + tr.creds.GetDeviceID()
	sas, err := tr.creds.Token(resource, lifetime)
	if err != nil {
		return err
	}

	// Send put-token request
	to := "$cbs"
	replyTo := "cbs"
	if err = sender.Send(ctx, &amqp.Message{
		Value: sas.String(),
		Properties: &amqp.MessageProperties{
			To:      &to,
			ReplyTo: &replyTo,
		},
		ApplicationProperties: map[string]interface{}{
			"operation": "put-token",
			"type":      "servicebus.windows.net:sastoken",
			"name":      resource,
		},
	}, nil); err != nil {
		return err
	}

	// Wait for response
	msg, err := receiver.Receive(ctx, nil)
	if err != nil {
		return err
	}
	if err = receiver.AcceptMessage(ctx, msg); err != nil {
		return err
	}

	// Check response status
	if err := checkCBSResponse(msg); err != nil {
		return err
	}

	tr.logDebug("CBS authentication successful")
	return nil
}

// tokenRefreshLoop periodically refreshes the CBS token.
func (tr *Transport) tokenRefreshLoop(sess *amqp.Session) {
	ticker := time.NewTimer(tokenUpdateInterval - tokenUpdateSpan)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := tr.putToken(ctx, sess, tokenUpdateInterval); err != nil {
				tr.logError("CBS token refresh failed: %s", err)
				cancel()
				tr.notifyConnectionStatus(false, err)
				return
			}
			cancel()
			ticker.Reset(tokenUpdateInterval - tokenUpdateSpan)
			tr.logDebug("CBS token refreshed")

		case <-tr.done:
			return
		}
	}
}

// checkCBSResponse checks if CBS response indicates success.
func checkCBSResponse(msg *amqp.Message) error {
	if msg.ApplicationProperties == nil {
		return nil
	}

	statusCode, ok := msg.ApplicationProperties["status-code"]
	if !ok {
		return nil
	}

	code, ok := statusCode.(int32)
	if !ok {
		return nil
	}

	if code < 200 || code >= 300 {
		desc := ""
		if d, ok := msg.ApplicationProperties["status-description"]; ok {
			desc = d.(string)
		}
		return &CBSError{Code: int(code), Description: desc}
	}

	return nil
}

// CBSError represents a CBS authentication error.
type CBSError struct {
	Code        int
	Description string
}

func (e *CBSError) Error() string {
	return e.Description
}
