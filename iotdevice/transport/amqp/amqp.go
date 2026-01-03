package amqp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/Azure/go-amqp"
	"github.com/bluesea251610e/iothub/common"
	"github.com/bluesea251610e/iothub/iotdevice/transport"
	"github.com/bluesea251610e/iothub/iotservice"
	"github.com/bluesea251610e/iothub/logger"
	"github.com/coder/websocket"
)

var ErrNotImplemented = errors.New("not implemented")

// ConnectionStatusHandler handles connection status changes
type ConnectionStatusHandler func(connected bool, err error)

// TransportOption is a transport configuration option.
type TransportOption func(tr *Transport)

// WithLogger sets logger for errors and warnings.
func WithLogger(l logger.Logger) TransportOption {
	return func(tr *Transport) {
		tr.logger = l
	}
}

// WithWebSocket enables AMQP over WebSocket on port 443.
func WithWebSocket(enable bool) TransportOption {
	return func(tr *Transport) {
		tr.webSocket = enable
	}
}

// WithConnectionStatusHandler sets the connection status callback.
func WithConnectionStatusHandler(handler ConnectionStatusHandler) TransportOption {
	return func(tr *Transport) {
		tr.connStatusHandler = handler
	}
}

// WithTLSConfig sets TLS configuration.
func WithTLSConfig(cfg *tls.Config) TransportOption {
	return func(tr *Transport) {
		tr.tlsCfg = cfg
	}
}

// New returns new AMQP transport.
func New(opts ...TransportOption) *Transport {
	tr := &Transport{
		done: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(tr)
	}
	return tr
}

// Transport is AMQP transport for Azure IoT Hub.
type Transport struct {
	mu   sync.RWMutex
	conn *amqp.Conn
	sess *amqp.Session

	creds  transport.Credentials
	logger logger.Logger
	tlsCfg *tls.Config

	webSocket         bool
	connStatusHandler ConnectionStatusHandler

	// CBS token refresh
	cbsSess *amqp.Session
	done    chan struct{}

	// D2C sender
	sendMu   sync.Mutex
	sendLink *amqp.Sender

	// C2D receiver
	recvMu   sync.Mutex
	recvLink *amqp.Receiver

	// Twin operations
	twinMu       sync.Mutex
	twinSender   *amqp.Sender
	twinReceiver *amqp.Receiver
	// twinCallbacks maps correlation ID to response channel
	twinCallbacks map[string]chan *amqp.Message
	// twinMux handles twin updates
	twinMux transport.TwinStateDispatcher

	// Direct methods
	methodMu       sync.Mutex
	methodReceiver *amqp.Receiver
	methodSender   *amqp.Sender
}

func (tr *Transport) SetLogger(l logger.Logger) {
	tr.logger = l
}

func (tr *Transport) Connect(ctx context.Context, creds transport.Credentials) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	if tr.conn != nil {
		return errors.New("already connected")
	}

	tr.creds = creds

	// Check if using X509 authentication
	useX509 := creds.GetCertificate() != nil

	// Build TLS config
	tlsCfg := tr.tlsCfg
	if tlsCfg == nil {
		tlsCfg = &tls.Config{
			RootCAs: common.RootCAs(),
		}
	}
	if crt := creds.GetCertificate(); crt != nil {
		tlsCfg.Certificates = append(tlsCfg.Certificates, *crt)
	}

	// Build connection options
	// When using X509, the TLS client certificate provides authentication (SASL EXTERNAL)
	connOpts := &amqp.ConnOptions{
		TLSConfig:  tlsCfg,
		Properties: map[string]any{"com.microsoft:client-version": "iothub-go-amqp/dev"},
	}

	// For X509, explicitly set SASL EXTERNAL mechanism
	// Empty string is used for TLS client certificate authentication
	if useX509 {
		connOpts.SASLType = amqp.SASLTypeExternal("")
	}

	var conn *amqp.Conn
	var err error

	if tr.webSocket {
		// AMQP over WebSocket
		conn, err = tr.dialWebSocket(ctx, creds.GetHostName(), connOpts)
	} else {
		// Standard AMQP over TLS
		addr := fmt.Sprintf("amqps://%s:5671", creds.GetHostName())
		conn, err = amqp.Dial(ctx, addr, connOpts)
	}

	if err != nil {
		tr.notifyConnectionStatus(false, err)
		return err
	}

	tr.conn = conn
	tr.logDebug("AMQP connection established to %s", creds.GetHostName())

	// Create main session
	sess, err := conn.NewSession(ctx, nil)
	if err != nil {
		conn.Close()
		tr.notifyConnectionStatus(false, err)
		return err
	}
	tr.sess = sess

	// Authentication based on credential type
	if useX509 {
		// X509 authentication - TLS client cert already authenticated via SASL EXTERNAL
		// No CBS authentication needed
		tr.logDebug("Using X509 authentication (SASL EXTERNAL via TLS client cert)")
	} else {
		// SAS authentication - use CBS (Claims-Based Security)
		if err := tr.startCBSAuth(ctx); err != nil {
			sess.Close(context.Background())
			conn.Close()
			tr.notifyConnectionStatus(false, err)
			return err
		}
	}

	tr.notifyConnectionStatus(true, nil)
	return nil
}

func (tr *Transport) dialWebSocket(ctx context.Context, host string, opts *amqp.ConnOptions) (*amqp.Conn, error) {
	// AMQP over WebSocket URL format for Azure IoT Hub
	wsURL := fmt.Sprintf("wss://%s:443/$iothub/websocket", host)

	// WebSocket dial options
	wsOpts := &websocket.DialOptions{
		Subprotocols: []string{"amqp"},
	}

	// Use TLS config from AMQP options if provided
	if opts != nil && opts.TLSConfig != nil {
		wsOpts.HTTPClient = nil // Will use default with TLS
	}

	// Dial WebSocket connection
	wsConn, _, err := websocket.Dial(ctx, wsURL, wsOpts)
	if err != nil {
		return nil, fmt.Errorf("websocket dial failed: %w", err)
	}

	// Wrap WebSocket as net.Conn using NetConn
	// NetConn requires a context and message type
	netConn := websocket.NetConn(ctx, wsConn, websocket.MessageBinary)

	// Create AMQP connection over the WebSocket net.Conn
	conn, err := amqp.NewConn(ctx, netConn, opts)
	if err != nil {
		wsConn.Close(websocket.StatusNormalClosure, "amqp connection failed")
		return nil, fmt.Errorf("amqp connection over websocket failed: %w", err)
	}

	return conn, nil
}

func (tr *Transport) notifyConnectionStatus(connected bool, err error) {
	if tr.connStatusHandler != nil {
		tr.connStatusHandler(connected, err)
	}
}

func (tr *Transport) logDebug(format string, args ...interface{}) {
	if tr.logger != nil {
		tr.logger.Debugf(format, args...)
	}
}

func (tr *Transport) logError(format string, args ...interface{}) {
	if tr.logger != nil {
		tr.logger.Errorf(format, args...)
	}
}

func (tr *Transport) logWarn(format string, args ...interface{}) {
	if tr.logger != nil {
		tr.logger.Warnf(format, args...)
	}
}

// Send sends a device-to-cloud message.
func (tr *Transport) Send(ctx context.Context, msg *common.Message) error {
	tr.sendMu.Lock()
	defer tr.sendMu.Unlock()

	if tr.sendLink == nil {
		// Create sender link for D2C messages
		addr := fmt.Sprintf("/devices/%s/messages/events", tr.creds.GetDeviceID())
		sender, err := tr.sess.NewSender(ctx, addr, nil)
		if err != nil {
			return err
		}
		tr.sendLink = sender
	}

	amqpMsg := tr.toAMQPMessage(msg)
	return tr.sendLink.Send(ctx, amqpMsg, nil)
}

func (tr *Transport) toAMQPMessage(msg *common.Message) *amqp.Message {
	props := &amqp.MessageProperties{}
	if msg.MessageID != "" {
		msgID := msg.MessageID
		props.MessageID = &msgID
	}
	if msg.CorrelationID != "" {
		props.CorrelationID = &msg.CorrelationID
	}
	if msg.To != "" {
		props.To = &msg.To
	}
	if msg.UserID != "" {
		props.UserID = []byte(msg.UserID)
	}
	if msg.ExpiryTime != nil {
		props.AbsoluteExpiryTime = msg.ExpiryTime
	}
	// Set content type and encoding as AMQP message properties
	if msg.ContentType != "" {
		props.ContentType = &msg.ContentType
	}
	if msg.ContentEncoding != "" {
		props.ContentEncoding = &msg.ContentEncoding
	}

	appProps := make(map[string]any)
	for k, v := range msg.Properties {
		appProps[k] = v
	}

	return &amqp.Message{
		Data:                  [][]byte{msg.Payload},
		Properties:            props,
		ApplicationProperties: appProps,
	}
}

// SubscribeEvents subscribes to cloud-to-device messages.
func (tr *Transport) SubscribeEvents(ctx context.Context, mux transport.MessageDispatcher) error {
	tr.recvMu.Lock()
	defer tr.recvMu.Unlock()

	if tr.recvLink != nil {
		return errors.New("already subscribed to events")
	}

	// Create receiver link for C2D messages
	addr := fmt.Sprintf("/devices/%s/messages/devicebound", tr.creds.GetDeviceID())
	receiver, err := tr.sess.NewReceiver(ctx, addr, nil)
	if err != nil {
		return err
	}
	tr.recvLink = receiver

	// Start receiving messages in background
	go tr.receiveEvents(ctx, mux)

	return nil
}

func (tr *Transport) receiveEvents(ctx context.Context, mux transport.MessageDispatcher) {
	for {
		select {
		case <-tr.done:
			return
		case <-ctx.Done():
			return
		default:
		}

		msg, err := tr.recvLink.Receive(ctx, nil)
		if err != nil {
			tr.logError("receive error: %s", err)
			return
		}

		commonMsg := tr.fromAMQPMessage(msg)
		mux.Dispatch(commonMsg)

		if err := tr.recvLink.AcceptMessage(ctx, msg); err != nil {
			tr.logError("accept message error: %s", err)
		}
	}
}

func (tr *Transport) fromAMQPMessage(msg *amqp.Message) *common.Message {
	result := &common.Message{
		Properties: make(map[string]string),
	}

	if len(msg.Data) > 0 {
		result.Payload = msg.Data[0]
	}

	if msg.Properties != nil {
		if msg.Properties.MessageID != nil {
			if mid, ok := msg.Properties.MessageID.(string); ok {
				result.MessageID = mid
			}
		}
		if msg.Properties.CorrelationID != nil {
			if cid, ok := msg.Properties.CorrelationID.(string); ok {
				result.CorrelationID = cid
			}
		}
		if msg.Properties.To != nil {
			result.To = *msg.Properties.To
		}
		if msg.Properties.UserID != nil {
			result.UserID = string(msg.Properties.UserID)
		}
		if msg.Properties.AbsoluteExpiryTime != nil {
			result.ExpiryTime = msg.Properties.AbsoluteExpiryTime
		}
	}

	for k, v := range msg.ApplicationProperties {
		if s, ok := v.(string); ok {
			result.Properties[k] = s
		}
	}

	return result
}

// RegisterDirectMethods registers direct method handler.
func (tr *Transport) RegisterDirectMethods(ctx context.Context, mux transport.MethodDispatcher) error {
	tr.methodMu.Lock()
	defer tr.methodMu.Unlock()

	if tr.methodReceiver != nil {
		return errors.New("already registered for direct methods")
	}

	// Azure IoT Hub AMQP Methods link address format:
	// amqps://{host}/devices/{deviceId}/methods/deviceBound
	// Reference: azure-iot-sdk-c/iothub_client/src/iothubtransport_amqp_messenger.c
	deviceID := tr.creds.GetDeviceID()
	hostName := tr.creds.GetHostName()
	moduleID := tr.creds.GetModuleID()

	var methodAddr string
	if moduleID != "" {
		methodAddr = fmt.Sprintf("amqps://%s/devices/%s/modules/%s/methods/deviceBound", hostName, deviceID, moduleID)
	} else {
		methodAddr = fmt.Sprintf("amqps://%s/devices/%s/methods/deviceBound", hostName, deviceID)
	}

	// Link attach properties required by Azure IoT Hub for Methods
	// Correlation ID for methods is the DeviceID (or DeviceID/ModuleID)
	correlationID := deviceID
	if moduleID != "" {
		correlationID = fmt.Sprintf("%s/%s", deviceID, moduleID)
	}

	linkProps := map[string]any{
		"com.microsoft:api-version":            "2019-10-01",
		"com.microsoft:channel-correlation-id": correlationID,
	}

	// Receiver Link (Requests): Source is Address, Target is "requests"
	receiverOpts := &amqp.ReceiverOptions{
		Properties:    linkProps,
		TargetAddress: "requests",
	}
	receiver, err := tr.sess.NewReceiver(ctx, methodAddr, receiverOpts)
	if err != nil {
		return err
	}
	tr.methodReceiver = receiver

	// Sender Link (Responses): Target is Address, Source is "responses"
	senderOpts := &amqp.SenderOptions{
		Properties:    linkProps,
		SourceAddress: "responses",
	}
	sender, err := tr.sess.NewSender(ctx, methodAddr, senderOpts)
	if err != nil {
		receiver.Close(ctx)
		return err
	}
	tr.methodSender = sender

	// Start handling methods in background
	go tr.handleMethods(ctx, mux)

	return nil
}

func (tr *Transport) handleMethods(ctx context.Context, mux transport.MethodDispatcher) {
	for {
		select {
		case <-tr.done:
			return
		case <-ctx.Done():
			return
		default:
		}

		msg, err := tr.methodReceiver.Receive(ctx, nil)
		if err != nil {
			tr.logError("method receive error: %s", err)
			return
		}

		// Extract method name from application properties
		methodName := ""
		if name, ok := msg.ApplicationProperties["IoThub-methodname"]; ok {
			methodName = name.(string)
		}

		// Dispatch method
		rc, respBody, err := mux.Dispatch(methodName, msg.GetData())
		if err != nil {
			tr.logError("method dispatch error: %s", err)
		}

		// Send response
		respMsg := &amqp.Message{
			Data: [][]byte{respBody},
			Properties: &amqp.MessageProperties{
				CorrelationID: msg.Properties.CorrelationID,
			},
			ApplicationProperties: map[string]any{
				"IoThub-status": int32(rc),
			},
		}

		if err := tr.methodSender.Send(ctx, respMsg, nil); err != nil {
			tr.logError("method response error: %s", err)
		}

		if err := tr.methodReceiver.AcceptMessage(ctx, msg); err != nil {
			tr.logError("accept method message error: %s", err)
		}
	}
}

// SubscribeTwinUpdates subscribes to twin desired property updates.
func (tr *Transport) SubscribeTwinUpdates(ctx context.Context, mux transport.TwinStateDispatcher) error {
	return tr.SubscribeTwinUpdatesImpl(ctx, mux)
}

// RetrieveTwinProperties retrieves twin properties.
func (tr *Transport) RetrieveTwinProperties(ctx context.Context) ([]byte, error) {
	return tr.RetrieveTwinPropertiesImpl(ctx)
}

// UpdateTwinProperties updates twin reported properties.
func (tr *Transport) UpdateTwinProperties(ctx context.Context, payload []byte) (int, error) {
	return tr.UpdateTwinPropertiesImpl(ctx, payload)
}

// GetBlobSharedAccessSignature is not available in the AMQP transport.
func (tr *Transport) GetBlobSharedAccessSignature(ctx context.Context, blobName string) (string, string, error) {
	return "", "", fmt.Errorf("unavailable in the AMQP transport, use HTTP")
}

// UploadToBlob is not available in the AMQP transport.
func (tr *Transport) UploadToBlob(ctx context.Context, sasURI string, file io.Reader, size int64) error {
	return fmt.Errorf("unavailable in the AMQP transport, use HTTP")
}

// NotifyUploadComplete is not available in the AMQP transport.
func (tr *Transport) NotifyUploadComplete(ctx context.Context, correlationID string, success bool, statusCode int, statusDescription string) error {
	return fmt.Errorf("unavailable in the AMQP transport, use HTTP")
}

// ListModules is not available in the AMQP transport.
func (tr *Transport) ListModules(ctx context.Context) ([]*iotservice.Module, error) {
	return nil, ErrNotImplemented
}

// CreateModule is not available in the AMQP transport.
func (tr *Transport) CreateModule(ctx context.Context, m *iotservice.Module) (*iotservice.Module, error) {
	return nil, ErrNotImplemented
}

// GetModule is not available in the AMQP transport.
func (tr *Transport) GetModule(ctx context.Context, moduleID string) (*iotservice.Module, error) {
	return nil, ErrNotImplemented
}

// UpdateModule is not available in the AMQP transport.
func (tr *Transport) UpdateModule(ctx context.Context, m *iotservice.Module) (*iotservice.Module, error) {
	return nil, ErrNotImplemented
}

// DeleteModule is not available in the AMQP transport.
func (tr *Transport) DeleteModule(ctx context.Context, m *iotservice.Module) error {
	return ErrNotImplemented
}

// Close closes the AMQP connection.
func (tr *Transport) Close() error {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	select {
	case <-tr.done:
		return nil
	default:
		close(tr.done)
	}

	tr.notifyConnectionStatus(false, nil)

	if tr.sendLink != nil {
		tr.sendLink.Close(context.Background())
	}
	if tr.recvLink != nil {
		tr.recvLink.Close(context.Background())
	}
	if tr.methodReceiver != nil {
		tr.methodReceiver.Close(context.Background())
	}
	if tr.methodSender != nil {
		tr.methodSender.Close(context.Background())
	}
	if tr.cbsSess != nil {
		tr.cbsSess.Close(context.Background())
	}
	if tr.sess != nil {
		tr.sess.Close(context.Background())
	}
	if tr.conn != nil {
		tr.conn.Close()
		tr.logDebug("AMQP connection closed")
	}

	return nil
}

// Ensure Transport implements transport.Transport interface
var _ transport.Transport = (*Transport)(nil)
