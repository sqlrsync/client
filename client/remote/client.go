package remote

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

const (
	SQLRSYNC_CONFIG = 0x51 // Send to keys and replicaID
)

// TrafficInspector provides traffic inspection and protocol message detection
type TrafficInspector struct {
	logger *zap.Logger
	depth  int
}

// NewTrafficInspector creates a new traffic inspector
func NewTrafficInspector(logger *zap.Logger, depth int) *TrafficInspector {
	if depth <= 0 {
		depth = 8 // default inspection depth
	}
	return &TrafficInspector{
		logger: logger,
		depth:  depth,
	}
}

// InspectOutbound logs outbound traffic (Go → Remote) and returns true if it's ORIGIN_END
func (t *TrafficInspector) InspectOutbound(data []byte, enableLogging bool) bool {
	if len(data) == 0 {
		return false
	}

	msgType := t.parseMessageType(data)

	if enableLogging {
		inspectionSize := t.depth
		if len(data) < inspectionSize {
			inspectionSize = len(data)
		}
		header := data[:inspectionSize]

		t.logger.Info("Traffic OUT (Go → Remote)",
			zap.String("messageType", msgType),
			zap.Int("totalBytes", len(data)),
			zap.String("header", fmt.Sprintf("%x", header)))
	}

	// Return whether this is an ORIGIN_END message
	return msgType == "ORIGIN_END"
}

// InspectInbound logs inbound traffic (Remote → Go) and returns true if it's ORIGIN_END
func (t *TrafficInspector) InspectInbound(data []byte, enableLogging bool) bool {
	if len(data) == 0 {
		return false
	}

	msgType := t.parseMessageType(data)

	if enableLogging {
		inspectionSize := t.depth
		if len(data) < inspectionSize {
			inspectionSize = len(data)
		}
		header := data[:inspectionSize]

		t.logger.Info("Traffic IN (Remote → Go)",
			zap.String("messageType", msgType),
			zap.Int("totalBytes", len(data)),
			zap.String("header", fmt.Sprintf("%x", header)))
	}

	// Return whether this is an ORIGIN_END message
	return msgType == "ORIGIN_END"
}

// LogWebSocketTraffic logs raw WebSocket traffic for debugging
func (t *TrafficInspector) LogWebSocketTraffic(data []byte, direction string, enableLogging bool) {
	if !enableLogging || len(data) == 0 {
		return
	}

	inspectionSize := t.depth
	if len(data) < inspectionSize {
		inspectionSize = len(data)
	}

	t.logger.Info(fmt.Sprintf("WebSocket %s", direction),
		zap.Int("totalBytes", len(data)),
		zap.String("preview", fmt.Sprintf("%x", data[:inspectionSize])))
}

// parseMessageType attempts to identify the SQLite rsync message type
func (t *TrafficInspector) parseMessageType(data []byte) string {
	if len(data) == 0 {
		return "EMPTY"
	}

	// SQLite rsync protocol is binary-based with single-byte message types
	firstByte := data[0]

	// Check for binary protocol messages based on the first byte
	switch firstByte {
	case 0x41: // ORIGIN_BEGIN
		return "ORIGIN_BEGIN"
	case 0x42: // ORIGIN_END
		return "ORIGIN_END"
	case 0x43: // ORIGIN_ERROR
		return "ORIGIN_ERROR"
	case 0x44: // ORIGIN_PAGE
		return "ORIGIN_PAGE"
	case 0x45: // ORIGIN_TXN
		return "ORIGIN_TXN"
	case 0x46: // ORIGIN_MSG
		return "ORIGIN_MSG"
	case 0x47: // ORIGIN_DETAIL
		return "ORIGIN_DETAIL"
	case 0x48: // ORIGIN_READY
		return "ORIGIN_READY"
	case 0x61: // REPLICA_BEGIN
		return "REPLICA_BEGIN"
	case 0x62: // REPLICA_ERROR
		return "REPLICA_ERROR"
	case 0x63: // REPLICA_END
		return "REPLICA_END"
	case 0x64: // REPLICA_HASH
		return "REPLICA_HASH"
	case 0x65: // REPLICA_READY
		return "REPLICA_READY"
	case 0x66: // REPLICA_MSG
		return "REPLICA_MSG"
	case 0x67: // REPLICA_CONFIG
		return "REPLICA_CONFIG"
	case 0x51:
		return "SQLRSYNC_CONFIG"
	default:
		// For unknown messages, classify by first byte
		if firstByte >= 32 && firstByte <= 126 {
			return fmt.Sprintf("TEXT_%c", firstByte)
		}
		return fmt.Sprintf("BINARY_0x%02x", firstByte)
	}
}

// Config holds the configuration for the remote WebSocket client
type Config struct {
	ServerURL               string
	Version                 string
	ReplicaID               string
	SetPublic               bool // for PUSH 
	Timeout                 int // in milliseconds
	Logger                  *zap.Logger
	EnableTrafficInspection bool // Enable detailed traffic logging
	InspectionDepth         int  // How many bytes to inspect (default: 32)
	PingPong                bool
	AuthToken               string
	SendConfigCmd           bool // the -sqlrsync file doesn't exist, so make a token
	LocalHostname           string
	LocalAbsolutePath       string
}

// Client handles WebSocket communication with the remote server
type Client struct {
	config *Config
	logger *zap.Logger
	conn   *websocket.Conn
	mu     sync.RWMutex
	ctx    context.Context
	cancel context.CancelFunc

	// Message queues for bridging with local client
	readQueue  chan []byte
	writeQueue chan []byte

	// Error handling and connection state
	lastError     error
	errorMu       sync.RWMutex
	connected     bool
	connectedMu   sync.RWMutex
	reconnectChan chan struct{}

	// Graceful shutdown coordination
	wg      sync.WaitGroup
	closed  bool
	closeMu sync.Mutex

	// Traffic inspection
	inspector *TrafficInspector

	// Sync completion detection
	syncCompleted bool
	syncMu        sync.RWMutex

	// sqlrsync specific
	NewPullKey  string
	NewPushKey  string
	ReplicaID   string
	ReplicaPath string
	SetPublic   bool
}

// New creates a new remote WebSocket client
func New(config *Config) (*Client, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	if config.Logger == nil {
		return nil, fmt.Errorf("logger cannot be nil")
	}

	// Set default inspection depth if not specified
	inspectionDepth := config.InspectionDepth
	if inspectionDepth <= 0 {
		inspectionDepth = 32
	}

	ctx, cancel := context.WithCancel(context.Background())

	client := &Client{
		config:        config,
		logger:        config.Logger,
		ctx:           ctx,
		cancel:        cancel,
		readQueue:     make(chan []byte, 8202),
		writeQueue:    make(chan []byte, 100),
		reconnectChan: make(chan struct{}, 1),
		inspector:     NewTrafficInspector(config.Logger, inspectionDepth),
	}
	return client, nil
}

// Connect establishes WebSocket connection to the remote server
func (c *Client) Connect() error {
	c.logger.Info("Connecting to remote server", zap.String("url", c.config.ServerURL))

	u, err := url.Parse(c.config.ServerURL)
	if err != nil || !strings.HasPrefix(u.Scheme, "ws") {
		fmt.Println("Server should be in the format: wss://server.com")
		return fmt.Errorf("invalid server URL: %w", err)
	}

	// Set up WebSocket dialer with timeout
	dialer := websocket.Dialer{
		HandshakeTimeout: time.Duration(c.config.Timeout) * time.Millisecond,
	}

	// Create connection context with timeout
	connectCtx, connectCancel := context.WithTimeout(c.ctx, time.Duration(c.config.Timeout)*time.Millisecond)
	defer connectCancel()

	headers := http.Header{}
	if c.config.AuthToken == "" || len(c.config.AuthToken) <= 20 {
		return fmt.Errorf("invalid authtoken: %s", c.config.AuthToken)
	} else {
		headers.Set("Authorization", c.config.AuthToken)
	}

	if c.config.LocalHostname != "" {
		headers.Set("X-LocalHostname", c.config.LocalHostname)
	}
	if c.config.LocalAbsolutePath != "" {
		headers.Set("X-LocalAbsolutePath", c.config.LocalAbsolutePath)
	}
	if c.config.Version != "" {
		headers.Set("X-ReplicaVersion", strings.Replace(c.config.Version, "latest", "", 1))
	}
	if c.config.ReplicaID != "" {
		headers.Set("X-ReplicaID", c.config.ReplicaID)
	}

	if c.config.SetPublic {
		headers.Set("X-SetPublic", fmt.Sprintf("%t", c.config.SetPublic))
	}

	conn, response, err := dialer.DialContext(connectCtx, u.String(), headers)
	if err != nil {
		respStr, _ := io.ReadAll(response.Body)
		return fmt.Errorf("%s", respStr)
	}
	defer response.Body.Close()

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	c.setConnected(true)

	// Set up ping/pong handlers for connection health
	conn.SetPingHandler(func(data string) error {
		c.logger.Debug("Received ping from server")
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(8*time.Second))
	})

	conn.SetPongHandler(func(data string) error {
		c.logger.Debug("Received pong from server")
		return nil
	})

	// Start message handling goroutines
	c.wg.Add(2) // readLoop, writeLoop
	go c.readLoop()
	go c.writeLoop()
	if c.config.PingPong {
		c.logger.Debug("Using Server PingPong")
		c.wg.Add(1)
		go c.pingLoop()
	}

	c.logger.Info("WebSocket connection established successfully")
	return nil
}

// Read reads data from the remote server (blocking until data is available)
func (c *Client) Read(buffer []byte) (int, error) {
	c.logger.Debug("Attempting to read from remote", zap.Int("bufferSize", len(buffer)))

	// Check if client is closed
	c.closeMu.Lock()
	closed := c.closed
	c.closeMu.Unlock()

	if closed {
		return 0, nil
	}

	// Check if we have a connection error first
	if lastErr := c.GetLastError(); lastErr != nil {
		// If sync is completed and this is a normal closure, return immediately
		if c.isSyncCompleted() && websocket.IsCloseError(lastErr, websocket.CloseNoStatusReceived, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
			c.logger.Info("Sync completed and connection closed normally - exiting immediately")
			return 0, nil
		}
		// If it's a normal closure, return EOF instead of error
		if websocket.IsCloseError(lastErr, websocket.CloseNoStatusReceived, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
			c.logger.Debug("Connection closed normally, returning EOF")
			return 0, fmt.Errorf("connection lost")
		}
		return 0, fmt.Errorf("connection error: %w", lastErr)
	}

	select {
	case <-c.ctx.Done():
		return 0, fmt.Errorf("client context cancelled")
	case data, ok := <-c.readQueue:
		if !ok {
			// Channel closed, connection is done
			if c.isSyncCompleted() {
				c.logger.Debug("Read queue closed after sync completion")
				return 0, nil
			}
			return 0, fmt.Errorf("connection lost")
		}

		if len(data) > len(buffer) {
			return 0, fmt.Errorf("received data larger than buffer: got %d, buffer %d", len(data), len(buffer))
		}

		bytesRead := copy(buffer, data)
		c.logger.Debug("Read data from remote", zap.Int("bytes", bytesRead))

		// Inspect inbound traffic and detect ORIGIN_END
		isOriginEnd := c.inspector.InspectInbound(data, c.config.EnableTrafficInspection)
		if isOriginEnd {
			c.logger.Info("ORIGIN_END received from server - sync completing")
			c.setSyncCompleted(true)
		}

		return bytesRead, nil
	case <-time.After(func() time.Duration {
		// Use a much shorter timeout if sync is completed
		if c.isSyncCompleted() {
			return 100 * time.Millisecond
		}
		return 9 * time.Second
	}()):
		// Check if connection is still alive
		if !c.isConnected() {
			return 0, fmt.Errorf("connection lost")
		}
		// If sync is completed, don't wait long
		if c.isSyncCompleted() {
			return 0, nil
		}
		return 0, fmt.Errorf("read timeout")
	}
}

// setSyncCompleted marks that the sync has completed (thread-safe)
func (c *Client) setSyncCompleted(completed bool) {
	c.syncMu.Lock()
	c.syncCompleted = completed
	c.syncMu.Unlock()
}

// isSyncCompleted returns whether the sync has completed
func (c *Client) isSyncCompleted() bool {
	c.syncMu.RLock()
	defer c.syncMu.RUnlock()
	return c.syncCompleted
}

// handleOutboundTraffic inspects outbound data and handles sync completion detection
func (c *Client) handleOutboundTraffic(data []byte) {
	// Always inspect for protocol messages (sync completion detection)
	isOriginEnd := c.inspector.InspectOutbound(data, c.config.EnableTrafficInspection)
	if isOriginEnd {
		c.logger.Info("ORIGIN_END detected - sync completing")
		c.setSyncCompleted(true)
	}
}

// Write sends data to the remote server
func (c *Client) Write(data []byte) error {
	c.logger.Debug("Writing data to remote", zap.Int("bytes", len(data)))

	// Handle traffic inspection and sync completion detection
	c.handleOutboundTraffic(data)

	// Make a copy of data to avoid race conditions
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	select {
	case <-c.ctx.Done():
		return fmt.Errorf("client context cancelled")
	case c.writeQueue <- dataCopy:
		c.logger.Debug("Data queued for writing", zap.Int("bytes", len(dataCopy)))
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("write queue timeout")
	}
}

// Close closes the WebSocket connection and cleans up resources
func (c *Client) Close() {
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return
	}
	c.closed = true
	c.closeMu.Unlock()

	c.logger.Info("Closing remote client")

	// Cancel context to signal all goroutines to stop
	c.cancel()

	// Close the WebSocket connection
	c.mu.Lock()
	if c.conn != nil {
		// Send close message
		c.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(5*time.Second))
		c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()

	c.setConnected(false)

	// Wait for all goroutines to finish
	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	// Wait with timeout
	select {
	case <-done:
		c.logger.Debug("All goroutines terminated successfully")
	case <-time.After(10 * time.Second):
		c.logger.Warn("Timeout waiting for goroutines to terminate")
	}

	// Close channels safely
	c.safeCloseChannels()

	c.logger.Info("Remote client closed")
}

// safeCloseChannels closes channels if they're not already closed
func (c *Client) safeCloseChannels() {
	// Close readQueue if not already closed
	func() {
		defer func() {
			if r := recover(); r != nil {
				// Channel was already closed, ignore
			}
		}()
		close(c.readQueue)
	}()

	// Close writeQueue if not already closed
	func() {
		defer func() {
			if r := recover(); r != nil {
				// Channel was already closed, ignore
			}
		}()
		close(c.writeQueue)
	}()

	// Close reconnectChan if not already closed
	func() {
		defer func() {
			if r := recover(); r != nil {
				// Channel was already closed, ignore
			}
		}()
		close(c.reconnectChan)
	}()
}

// Reconnect attempts to reconnect to the WebSocket server
func (c *Client) Reconnect() error {
	c.logger.Info("Attempting to reconnect to remote server")

	// Close existing connection if any
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.mu.Unlock()

	c.setConnected(false)

	// Clear any previous error
	c.setError(nil)

	// Recreate queues if they were closed
	select {
	case <-c.readQueue:
	default:
		// Channel is open
	}

	// Attempt to reconnect
	return c.Connect()
}

// pingLoop sends periodic ping messages to keep the connection alive
func (c *Client) pingLoop() {
	defer c.wg.Done()
	c.logger.Debug("Starting ping loop")
	defer c.logger.Debug("Ping loop terminated")

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Check connection status more frequently to exit quickly when disconnected
	statusTicker := time.NewTicker(1 * time.Second)
	defer statusTicker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-statusTicker.C:
			// Quick exit if connection is lost
			if !c.isConnected() {
				c.logger.Debug("Ping loop exiting due to disconnection")
				return
			}
		case <-ticker.C:
			c.mu.RLock()
			conn := c.conn
			c.mu.RUnlock()

			if conn == nil || !c.isConnected() {
				return
			}

			err := conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(10*time.Second))
			if err != nil {
				c.logger.Error("Failed to send ping", zap.Error(err))
				c.setError(err)
				c.setConnected(false)
				return
			}
			c.logger.Debug("Sent ping to server")
		}
	}
}

// GetLastError returns the last error encountered
func (c *Client) GetLastError() error {
	c.errorMu.RLock()
	defer c.errorMu.RUnlock()
	return c.lastError
}

// setError sets the last error (thread-safe)
func (c *Client) setError(err error) {
	c.errorMu.Lock()
	c.lastError = err
	c.errorMu.Unlock()
}

// isConnected returns the current connection state
func (c *Client) isConnected() bool {
	c.connectedMu.RLock()
	defer c.connectedMu.RUnlock()
	return c.connected
}

// setConnected sets the connection state
func (c *Client) setConnected(connected bool) {
	c.connectedMu.Lock()
	c.connected = connected
	c.connectedMu.Unlock()
}

// readLoop handles incoming WebSocket messages
func (c *Client) readLoop() {
	defer c.wg.Done()
	c.logger.Debug("Starting read loop")
	defer func() {
		c.logger.Debug("Read loop terminated")
		c.setConnected(false)
	}()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			c.mu.RLock()
			conn := c.conn
			c.mu.RUnlock()

			if conn == nil || c.isSyncCompleted() {
				c.setConnected(false)
				return
			}

			// Set read deadline
			conn.SetReadDeadline(time.Now().Add(9 * time.Second))

			messageType, data, err := conn.ReadMessage()
			if err != nil {
				// Check if this is an expected/normal connection closure
				if websocket.IsCloseError(err,
					websocket.CloseNoStatusReceived, // 1005 - normal close from server after ORIGIN_END
					websocket.CloseNormalClosure,    // 1000 - normal closure
					websocket.CloseGoingAway) {      // 1001 - endpoint going away
					c.logger.Info("WebSocket connection closed normally", zap.Error(err))
				} else {
					// Any other error is unexpected
					c.logger.Error("WebSocket read error", zap.Error(err))
				}
				c.setError(err)
				c.setConnected(false)

				// If sync is completed and this is a normal closure, close read queue immediately
				if c.isSyncCompleted() && websocket.IsCloseError(err,
					websocket.CloseNoStatusReceived,
					websocket.CloseNormalClosure,
					websocket.CloseGoingAway) {
					c.logger.Debug("Sync completed - closing read queue immediately")
					// Close the read queue to signal no more data
					close(c.readQueue)
				}

				// Only signal reconnection for truly unexpected errors (not normal closures)
				if !websocket.IsCloseError(err,
					websocket.CloseNoStatusReceived,
					websocket.CloseNormalClosure,
					websocket.CloseGoingAway) {
					select {
					case c.reconnectChan <- struct{}{}:
					default:
					}
				}
				return
			}

			if messageType == websocket.TextMessage {
				accessKeyLength := 22
				replicaIDLength := 18
				readPullKeyResp := "NEWPULLKEY="
				readPushKeyResp := "NEWPUSHKEY="
				replicaIDResp := "REPLICAID="
				replicaPathResp := "REPLICAPATH="
				// Handle text messages for NEWPULLKEY, NEWPUSHKEY, REPLICAID
				// Example: "NEWPULLKEY=xxxxxxxxxxxxxxxxxxxxxx"
				strData := string(data)
				if (len(data) >= len(readPullKeyResp)+accessKeyLength) && strings.HasPrefix(strData, readPullKeyResp) {
					c.NewPullKey = strData[len(readPullKeyResp):]
					c.logger.Debug("📥 Received new Pull Key:", zap.String("key", c.NewPullKey))
				} else if (len(data) >= len(readPushKeyResp)+accessKeyLength) && strings.HasPrefix(strData, readPushKeyResp) {

					c.NewPushKey = strData[len(readPushKeyResp):]
					c.logger.Debug("📥 Received new Push Key:", zap.String("key", c.NewPushKey))
				} else if (len(data) >= len(replicaIDResp)+replicaIDLength) && strings.HasPrefix(strData, replicaIDResp) {
					c.ReplicaID = strData[len(replicaIDResp):]
					c.logger.Debug("📥 Received Replica ID:", zap.String("id", c.ReplicaID))
				} else if (len(data) >= len(replicaPathResp)) && strings.HasPrefix(strData, replicaPathResp) {

					c.ReplicaPath = strData[len(replicaPathResp):]
					c.logger.Debug("📥 Received new Replica Path:", zap.String("path", c.ReplicaPath))
				}
				continue
			}

			if messageType != websocket.BinaryMessage {
				c.logger.Warn("Received non-binary message", zap.Int("messageType", messageType))
				continue
			}

			c.logger.Debug("Received message from remote", zap.Int("bytes", len(data)))

			c.inspector.LogWebSocketTraffic(data, "IN (Server → Client)", c.config.EnableTrafficInspection)

			// Check if this is ORIGIN_END to detect sync completion early
			msgType := c.inspector.parseMessageType(data)
			if msgType == "ORIGIN_END" {
				c.logger.Info("ORIGIN_END detected in read loop - sync will complete")
				c.setSyncCompleted(true)
			}

			// Queue the data for reading
			select {
			case c.readQueue <- data:
				c.logger.Debug("Data queued for reading", zap.Int("bytes", len(data)))
			case <-c.ctx.Done():
				return
			default:
				c.logger.Warn("Read queue full, dropping message", zap.Int("bytes", len(data)))
			}
		}
	}
}

// writeLoop handles outgoing WebSocket messages
func (c *Client) writeLoop() {
	defer c.wg.Done()
	c.logger.Debug("Starting write loop")
	defer func() {
		c.logger.Debug("Write loop terminated")
		c.setConnected(false)
	}()

	for {
		select {
		case <-c.ctx.Done():
			return
		case data, ok := <-c.writeQueue:
			if !ok {
				c.logger.Debug("Write queue closed")
				return
			}

			c.mu.RLock()
			conn := c.conn
			c.mu.RUnlock()

			if conn == nil {
				c.logger.Error("Connection is nil in write loop")
				c.setConnected(false)
				return
			}

			// Set write deadline
			conn.SetWriteDeadline(time.Now().Add(30 * time.Second))

			// Inspect raw WebSocket outbound traffic
			c.inspector.LogWebSocketTraffic(data, "OUT (Client → Server)", c.config.EnableTrafficInspection)

			err := conn.WriteMessage(websocket.BinaryMessage, data)
			if err != nil {
				c.logger.Error("WebSocket write error", zap.Error(err))
				c.setError(err)
				c.setConnected(false)

				// Signal potential reconnection
				select {
				case c.reconnectChan <- struct{}{}:
				default:
				}
				return
			}

			if c.config.SendConfigCmd {
				conn.WriteMessage(websocket.BinaryMessage, []byte{SQLRSYNC_CONFIG})
				c.config.SendConfigCmd = false
				c.logger.Debug("🔑 Also asked for keys and replicaID.")
			}

			c.logger.Debug("Sent message to remote", zap.Int("bytes", len(data)))
		}
	}
}

func (c *Client) GetNewPullKey() string {
	return c.NewPullKey
}

func (c *Client) GetNewPushKey() string {
	return c.NewPushKey
}

func (c *Client) GetReplicaID() string {
	return c.ReplicaID
}
func (c *Client) GetReplicaPath() string {
	return c.ReplicaPath
}
