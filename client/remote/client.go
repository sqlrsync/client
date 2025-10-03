package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

const (
	SQLRSYNC_CONFIG            = 0x51 // Send to keys and replicaID
	SQLRSYNC_NEWREPLICAVERSION = 0x52 // New version available
	SQLRSYNC_KEYREQUEST        = 0x53 // request keys
	SQLRSYNC_COMMITMESSAGE     = 0x54 // commit message
)

// ProgressPhase represents the current phase of the sync operation
type ProgressPhase int

const (
	PhaseInitializing ProgressPhase = iota
	PhaseNegotiating
	PhaseTransferring
	PhaseCompleting
	PhaseCompleted
)

// SyncDirection represents the direction of the sync operation
type SyncDirection int

const (
	DirectionUnknown SyncDirection = iota
	DirectionPush                  // Local → Remote (ORIGIN_* messages outbound)
	DirectionPull                  // Remote → Local (REPLICA_* messages inbound)
)

// ProgressEventType represents different types of progress events
type ProgressEventType int

const (
	EventSyncStart ProgressEventType = iota
	EventNegotiationComplete
	EventPageSent
	EventPageReceived
	EventPageConfirmed
	EventSyncComplete
	EventError
)

// SyncProgress tracks the current state of a sync operation
type SyncProgress struct {
	// Basic metrics
	TotalPages int
	PageSize   int
	TotalBytes int64

	// Progress counters
	PagesSent        int // ORIGIN_PAGE count
	PagesReceived    int // REPLICA_PAGE count
	PagesConfirmed   int // REPLICA_HASH/ORIGIN_HASH count
	BytesTransferred int64

	// Timing
	StartTime  time.Time
	LastUpdate time.Time

	// State
	Phase     ProgressPhase
	Direction SyncDirection

	// Calculated fields
	PercentComplete float64
	EstimatedETA    time.Duration
	PagesPerSecond  float64
}

// SyncProgressEvent represents a progress event
type SyncProgressEvent struct {
	Type     ProgressEventType
	Progress *SyncProgress
	Message  string
	Error    error
}

// ProgressFormat defines how progress should be displayed
type ProgressFormat int

const (
	FormatSimple   ProgressFormat = iota // Just percentage
	FormatDetailed                       // Full progress bar with details
	FormatJSON                           // Machine-readable JSON
)

// ProgressConfig configures progress reporting behavior
type ProgressConfig struct {
	Enabled        bool
	Format         ProgressFormat
	UpdateRate     time.Duration // Minimum time between updates
	ShowETA        bool
	ShowBytes      bool
	ShowPages      bool
	PagesPerUpdate int // Update every N pages (default: 10)
}

// ProgressCallback is called when progress events occur
type ProgressCallback func(event SyncProgressEvent)

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
func (t *TrafficInspector) InspectOutbound(data []byte, enableLogging bool) string {
	if len(data) == 0 {
		return ""
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
	return msgType
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

// InspectForProgress analyzes messages for progress tracking and calls the callback
func (t *TrafficInspector) InspectForProgress(data []byte, direction string, callback ProgressCallback, enableLogging bool) {
	if len(data) == 0 || callback == nil {
		return
	}

	msgType := t.parseMessageType(data)

	// Log the message if enabled
	if enableLogging {
		inspectionSize := t.depth
		if len(data) < inspectionSize {
			inspectionSize = len(data)
		}
		header := data[:inspectionSize]

		t.logger.Debug(fmt.Sprintf("Progress inspection %s", direction),
			zap.String("messageType", msgType),
			zap.Int("totalBytes", len(data)),
			zap.String("header", fmt.Sprintf("%x", header)))
	}

	switch msgType {
	case "ORIGIN_BEGIN":
		if progress := t.parseBeginMessage(data, DirectionPush); progress != nil {
			callback(SyncProgressEvent{
				Type:     EventSyncStart,
				Progress: progress,
				Message:  fmt.Sprintf("Starting sync: %d pages (%d bytes) to push", progress.TotalPages, progress.TotalBytes),
			})
		}
	case "REPLICA_BEGIN":
		if progress := t.parseBeginMessage(data, DirectionPull); progress != nil {
			callback(SyncProgressEvent{
				Type:     EventSyncStart,
				Progress: progress,
				Message:  fmt.Sprintf("Starting sync: %d pages (%d bytes) to pull", progress.TotalPages, progress.TotalBytes),
			})
		}
	case "ORIGIN_PAGE":
		callback(SyncProgressEvent{
			Type:    EventPageSent,
			Message: "Page sent",
		})
	case "REPLICA_PAGE":
		callback(SyncProgressEvent{
			Type:    EventPageReceived,
			Message: "Page received",
		})
	case "REPLICA_HASH", "ORIGIN_HASH":
		callback(SyncProgressEvent{
			Type:    EventPageConfirmed,
			Message: "Page confirmed",
		})
	case "ORIGIN_READY", "REPLICA_READY":
		callback(SyncProgressEvent{
			Type:    EventNegotiationComplete,
			Message: "Protocol negotiation complete",
		})
	case "ORIGIN_END", "REPLICA_END":
		callback(SyncProgressEvent{
			Type:    EventSyncComplete,
			Message: "Sync operation completed",
		})
	}
}

// parseBeginMessage attempts to parse ORIGIN_BEGIN or REPLICA_BEGIN message payload
func (t *TrafficInspector) parseBeginMessage(data []byte, direction SyncDirection) *SyncProgress {
	if len(data) < 9 { // Need at least message type + 8 bytes for basic info
		return nil
	}

	// Log the raw message bytes for debugging
	minLen := len(data)
	if minLen > 16 {
		minLen = 16
	}
	t.logger.Info("Parsing BEGIN message",
		zap.String("direction", func() string {
			if direction == DirectionPush {
				return "PUSH"
			}
			return "PULL"
		}()),
		zap.Int("messageLength", len(data)),
		zap.String("rawBytes", fmt.Sprintf("%x", data[:minLen])))

	// SQLite rsync protocol structure for BEGIN messages (simplified parsing)
	// This is a best-effort parse - exact structure may vary
	// Byte 0: Message type (0x41 for ORIGIN_BEGIN, 0x61 for REPLICA_BEGIN)
	// Bytes 1-4: Total pages (little-endian uint32)
	// Bytes 5-8: Page size (little-endian uint32)

	totalPages := int(data[1]) | int(data[2])<<8 | int(data[3])<<16 | int(data[4])<<24
	pageSize := int(data[5]) | int(data[6])<<8 | int(data[7])<<16 | int(data[8])<<24

	t.logger.Info("Parsed values from BEGIN message",
		zap.Int("totalPages", totalPages),
		zap.Int("pageSize", pageSize),
		zap.String("bytes1-4", fmt.Sprintf("%02x %02x %02x %02x", data[1], data[2], data[3], data[4])),
		zap.String("bytes5-8", fmt.Sprintf("%02x %02x %02x %02x", data[5], data[6], data[7], data[8])))

	// Sanity check the parsed values - allow smaller page sizes like 4096
	if totalPages <= 0 || totalPages > 1000000 || pageSize <= 0 || pageSize > 65536 {
		t.logger.Warn("Parsed BEGIN message with suspicious values",
			zap.Int("totalPages", totalPages),
			zap.Int("pageSize", pageSize))
		return nil
	}

	progress := &SyncProgress{
		TotalPages:      totalPages,
		PageSize:        pageSize,
		TotalBytes:      int64(totalPages) * int64(pageSize),
		Phase:           PhaseInitializing,
		Direction:       direction,
		StartTime:       time.Now(),
		LastUpdate:      time.Now(),
		PercentComplete: 0.0,
	}

	t.logger.Info("Parsed sync parameters",
		zap.Int("totalPages", totalPages),
		zap.Int("pageSize", pageSize),
		zap.Int64("totalBytes", progress.TotalBytes),
		zap.String("direction", func() string {
			if direction == DirectionPush {
				return "PUSH"
			}
			return "PULL"
		}()))

	return progress
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
	case 0x52:
		return "SQLRSYNC_NEWREPLICAVERSION"
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
	Subscribe               bool
	SetVisibility           int // for PUSH
	CommitMessage           []byte
	Timeout                 int // in milliseconds
	Logger                  *zap.Logger
	EnableTrafficInspection bool // Enable detailed traffic logging
	InspectionDepth         int  // How many bytes to inspect (default: 32)
	PingPong                bool
	AuthToken               string
	SendKeyRequest          bool // the -sqlrsync file doesn't exist, so make a token

	SendConfigCmd     bool // we don't have the version number or remote path
	LocalHostname     string
	LocalAbsolutePath string
	WsID              string // Workspace ID for X-ClientID header

	// Progress tracking
	ProgressConfig   *ProgressConfig
	ProgressCallback ProgressCallback
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
	NewPullKey     string
	NewPushKey     string
	ReplicaID      string
	Version        string
	ReplicaPath    string
	SetVisibility  int
	newVersionChan chan struct{}

	// Progress tracking
	progress         *SyncProgress
	progressMu       sync.RWMutex
	lastProgressSent time.Time
	pagesSinceUpdate int
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

	// Set default progress config if progress is enabled but config is nil
	if config.ProgressCallback != nil && config.ProgressConfig == nil {
		config.ProgressConfig = &ProgressConfig{
			Enabled:        true,
			Format:         FormatSimple,
			UpdateRate:     500 * time.Millisecond,
			ShowETA:        true,
			ShowBytes:      true,
			ShowPages:      true,
			PagesPerUpdate: 10,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	client := &Client{
		config:         config,
		logger:         config.Logger,
		ctx:            ctx,
		cancel:         cancel,
		readQueue:      make(chan []byte, 3),
		writeQueue:     make(chan []byte, 5),
		reconnectChan:  make(chan struct{}, 1),
		inspector:      NewTrafficInspector(config.Logger, inspectionDepth),
		newVersionChan: make(chan struct{}, 1),
	}
	return client, nil
}

// Progress tracking methods

// initProgress initializes progress tracking with the given parameters
func (c *Client) initProgress(totalPages, pageSize int, direction SyncDirection) {
	c.progressMu.Lock()
	defer c.progressMu.Unlock()

	c.progress = &SyncProgress{
		TotalPages:      totalPages,
		PageSize:        pageSize,
		TotalBytes:      int64(totalPages) * int64(pageSize),
		Phase:           PhaseInitializing,
		Direction:       direction,
		StartTime:       time.Now(),
		LastUpdate:      time.Now(),
		PercentComplete: 0.0,
	}
	c.lastProgressSent = time.Now()
	c.pagesSinceUpdate = 0
}

// updateProgress updates progress state and potentially calls the callback
func (c *Client) updateProgress(eventType ProgressEventType, message string) {
	if c.config.ProgressCallback == nil || c.config.ProgressConfig == nil || !c.config.ProgressConfig.Enabled {
		return
	}

	c.progressMu.Lock()
	defer c.progressMu.Unlock()

	if c.progress == nil {
		return
	}

	now := time.Now()
	c.progress.LastUpdate = now

	// Update counters based on event type
	switch eventType {
	case EventPageSent:
		c.progress.PagesSent++
		c.pagesSinceUpdate++
	case EventPageReceived:
		c.progress.PagesReceived++
		c.pagesSinceUpdate++
	case EventPageConfirmed:
		c.progress.PagesConfirmed++
	case EventNegotiationComplete:
		c.progress.Phase = PhaseTransferring
	case EventSyncComplete:
		c.progress.Phase = PhaseCompleted
		c.progress.PercentComplete = 100.0
	}

	// Calculate derived metrics
	c.calculateProgressMetrics()

	// Determine if we should send an update
	shouldUpdate := c.shouldSendProgressUpdate(eventType, now)

	if shouldUpdate {
		c.sendProgressUpdate(eventType, message)
		c.lastProgressSent = now
		c.pagesSinceUpdate = 0
	}
}

// calculateProgressMetrics updates calculated fields in progress
func (c *Client) calculateProgressMetrics() {
	if c.progress == nil {
		return
	}

	// Calculate progress percentage
	var completedPages int
	if c.progress.Direction == DirectionPush {
		completedPages = c.progress.PagesSent
	} else {
		completedPages = c.progress.PagesReceived
	}

	if c.progress.TotalPages > 0 {
		c.progress.PercentComplete = float64(completedPages) / float64(c.progress.TotalPages) * 100.0
	}

	// Calculate bytes transferred
	c.progress.BytesTransferred = int64(completedPages) * int64(c.progress.PageSize)

	// Calculate speed and ETA
	elapsed := time.Since(c.progress.StartTime)
	if elapsed > 0 {
		c.progress.PagesPerSecond = float64(completedPages) / elapsed.Seconds()

		if c.progress.PagesPerSecond > 0 {
			remainingPages := c.progress.TotalPages - completedPages
			c.progress.EstimatedETA = time.Duration(float64(remainingPages)/c.progress.PagesPerSecond) * time.Second
		}
	}
}

// shouldSendProgressUpdate determines if a progress update should be sent
func (c *Client) shouldSendProgressUpdate(eventType ProgressEventType, now time.Time) bool {
	// Always send for phase changes and completion
	if eventType == EventSyncStart || eventType == EventNegotiationComplete || eventType == EventSyncComplete || eventType == EventError {
		return true
	}

	// Check rate limiting
	if now.Sub(c.lastProgressSent) < c.config.ProgressConfig.UpdateRate {
		// But still send if we've hit the page threshold
		return c.pagesSinceUpdate >= c.config.ProgressConfig.PagesPerUpdate
	}

	// Send if enough time has passed and we have updates
	return c.pagesSinceUpdate > 0
}

// sendProgressUpdate calls the progress callback
func (c *Client) sendProgressUpdate(eventType ProgressEventType, message string) {
	if c.config.ProgressCallback == nil || c.progress == nil {
		return
	}

	// Create a copy of progress to avoid race conditions
	progressCopy := *c.progress

	event := SyncProgressEvent{
		Type:     eventType,
		Progress: &progressCopy,
		Message:  message,
	}

	// Call the callback in a goroutine to avoid blocking
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("Progress callback panicked", zap.Any("panic", r))
			}
		}()
		c.config.ProgressCallback(event)
	}()
}

// getProgress returns a copy of the current progress (thread-safe)
func (c *Client) GetProgress() *SyncProgress {
	c.progressMu.RLock()
	defer c.progressMu.RUnlock()

	if c.progress == nil {
		return nil
	}

	progressCopy := *c.progress
	return &progressCopy
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

	headers.Set("Authorization", c.config.AuthToken)

	if c.config.WsID != "" {
		headers.Set("X-ClientID", c.config.WsID)
	} else {
		c.logger.Fatal("No wsID provided for X-ClientID header")
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
	if c.config.SetVisibility != 0 {
		headers.Set("X-Visibility", strconv.Itoa(c.config.SetVisibility))
	}

	conn, response, err := dialer.DialContext(connectCtx, u.String(), headers)
	if err != nil {
		if response != nil {
			// Extract detailed error information from the response
			statusCode := response.StatusCode
			statusText := response.Status

			var respBodyStr string
			if response.Body != nil {
				respBytes, readErr := io.ReadAll(response.Body)
				response.Body.Close()
				if readErr == nil {
					respBodyStr = strings.TrimSpace(string(respBytes))
				}
			}

			// Create a clean error message
			var errorMsg strings.Builder
			errorMsg.WriteString(fmt.Sprintf("HTTP %d (%s)", statusCode, statusText))

			if respBodyStr != "" {
				errorMsg.WriteString(fmt.Sprintf(": %s", respBodyStr))
			}

			return fmt.Errorf("%s", errorMsg.String())
		}

		// Handle cases where response is nil (e.g., network errors, bad handshake)
		var errorMsg strings.Builder
		errorMsg.WriteString("Failed to connect to WebSocket")

		// Analyze the error type and provide helpful context
		errorStr := err.Error()
		if strings.Contains(errorStr, "bad handshake") {
			errorMsg.WriteString(" - WebSocket handshake failed")
			errorMsg.WriteString("\nThis could be due to:")
			errorMsg.WriteString("\n• Invalid server URL or endpoint")
			errorMsg.WriteString("\n• Server not supporting WebSocket connections")
			errorMsg.WriteString("\n• Network connectivity issues")
			errorMsg.WriteString("\n• Authentication problems")
		} else if strings.Contains(errorStr, "timeout") {
			errorMsg.WriteString(" - Connection timeout")
			errorMsg.WriteString("\nThe server may be overloaded or unreachable")
		} else if strings.Contains(errorStr, "refused") {
			errorMsg.WriteString(" - Connection refused")
			errorMsg.WriteString("\nThe server may be down or the port may be blocked")
		} else if strings.Contains(errorStr, "no such host") {
			errorMsg.WriteString(" - DNS resolution failed")
			errorMsg.WriteString("\nCheck the server hostname in your configuration")
		}

		errorMsg.WriteString(fmt.Sprintf("\nOriginal error: %v", err))

		return fmt.Errorf("%s", errorMsg.String())
	}
	defer response.Body.Close()

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	c.setConnected(true)

	// Set up ping/pong handlers for connection health
	conn.SetPingHandler(func(data string) error {
		c.logger.Debug("Received ping from server")
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
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

	// Check if connection is still alive first
	if !c.isConnected() {
		c.logger.Debug("Connection not active, returning immediately")
		// If sync completed normally, return success
		if c.isSyncCompleted() {
			return 0, nil
		}
		// Otherwise return connection lost
		return 0, fmt.Errorf("connection lost")
	}

	// Check if we have a connection error
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
			// In subscribe mode, continue reading for new version notifications
			if c.config.Subscribe {
				c.logger.Info("Subscribe mode: continuing to listen for new version notifications")
			}
		}

		// Handle progress tracking for inbound traffic
		if c.config.ProgressCallback != nil {
			c.inspector.InspectForProgress(data, "IN (Server → Client)", func(event SyncProgressEvent) {
				c.handleProgressEvent(event)
			}, c.config.EnableTrafficInspection)
		}

		return bytesRead, nil
	case <-time.After(func() time.Duration {
		// In subscribe mode, use very long timeout to accommodate hibernated connections
		if c.config.Subscribe {
			return 1 * time.Hour
		}
		// Use a longer timeout if sync is completed to allow final transaction processing
		if c.isSyncCompleted() {
			return 2 * time.Second
		}
		return 30 * time.Second
	}()):
		// Check if connection is still alive
		if !c.isConnected() {
			return 0, fmt.Errorf("connection lost")
		}
		// If sync is completed and not in subscribe mode, don't wait long
		if c.isSyncCompleted() {
			return 0, nil
		}
		// In subscribe mode, continue reading even after timeouts
		if c.config.Subscribe {
			return 0, nil // Return 0 bytes but no error, allowing caller to retry
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
	return c.syncCompleted && !c.config.Subscribe
}

// handleOutboundTraffic inspects outbound data and handles sync completion detection
func (c *Client) handleOutboundTraffic(data []byte) {
	// Always inspect for protocol messages (sync completion detection)
	outboundCommand := c.inspector.InspectOutbound(data, c.config.EnableTrafficInspection)
	if outboundCommand == "ORIGIN_END" {
		c.logger.Info("ORIGIN_END detected - sync completing")
		c.setSyncCompleted(true)
	}
	if outboundCommand == "ORIGIN_BEGIN" {
		if len(c.config.CommitMessage) > 0 {
			length := len(c.config.CommitMessage)
			// Encode length as 2 bytes (big-endian), 2 bytes is ~65k max
			lenBytes := []byte{
				byte(length >> 8),
				byte(length),
			}
			c.writeQueue <- append([]byte{SQLRSYNC_COMMITMESSAGE}, append(lenBytes, c.config.CommitMessage...)...)
			c.config.CommitMessage = nil
		}
	}

	// Handle progress tracking
	if c.config.ProgressCallback != nil {
		c.inspector.InspectForProgress(data, "OUT (Client → Server)", func(event SyncProgressEvent) {
			c.handleProgressEvent(event)
		}, c.config.EnableTrafficInspection)
	}
}

// handleProgressEvent processes progress events from the traffic inspector
func (c *Client) handleProgressEvent(event SyncProgressEvent) {
	switch event.Type {
	case EventSyncStart:
		if event.Progress != nil {
			c.initProgress(event.Progress.TotalPages, event.Progress.PageSize, event.Progress.Direction)
		}
		c.updateProgress(EventSyncStart, event.Message)
	case EventPageSent:
		c.updateProgress(EventPageSent, event.Message)
	case EventPageReceived:
		c.updateProgress(EventPageReceived, event.Message)
	case EventPageConfirmed:
		c.updateProgress(EventPageConfirmed, event.Message)
	case EventNegotiationComplete:
		c.updateProgress(EventNegotiationComplete, event.Message)
	case EventSyncComplete:
		c.updateProgress(EventSyncComplete, event.Message)
	case EventError:
		c.updateProgress(EventError, event.Message)
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
	case <-time.After(30 * time.Second):
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

	// Close the WebSocket connection gracefully
	c.mu.Lock()
	if c.conn != nil {
		// Send close message
		closeMessage := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
		err := c.conn.WriteControl(websocket.CloseMessage, closeMessage, time.Now().Add(5*time.Second))
		if err != nil {
			c.logger.Debug("Error sending close message", zap.Error(err))
		} else {
			c.logger.Debug("Sent WebSocket close message")
		}

		// Set a read deadline for the close handshake
		c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))

		// Wait for server's close response by reading until we get a close frame
		for {
			_, _, err := c.conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
					c.logger.Debug("Received close acknowledgment from server")
				} else {
					c.logger.Debug("Connection closed during close handshake", zap.Error(err))
				}
				break
			}
			// Keep reading until we get the close frame or timeout
		}

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

	// Use longer ping interval in subscribe mode to accommodate hibernated connections
	pingInterval := 5 * time.Second
	if c.config.Subscribe {
		pingInterval = 25 * time.Minute
		c.logger.Info("Subscribe mode: using 25-minute ping interval for hibernated connections")
	}

	ticker := time.NewTicker(pingInterval)
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

			// Use longer ping timeout in subscribe mode
			pingTimeout := 10 * time.Second
			if c.config.Subscribe {
				pingTimeout = 30 * time.Second
			}

			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(pingTimeout))
			if err != nil {
				c.logger.Error("Failed to send ping", zap.Error(err))
				c.setError(err)
				c.setConnected(false)
				return
			}

			if c.config.Subscribe {
				c.logger.Info("Sent ping to hibernated server connection")
			} else {
				c.logger.Debug("Sent ping to server")
			}
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

			if conn == nil {
				c.setConnected(false)
				return
			}

			// In subscribe mode, continue even after sync completion
			if c.isSyncCompleted() && !c.config.Subscribe {
				c.setConnected(false)
				return
			}

			// In subscribe mode, if connection failed, don't continue reading
			if c.config.Subscribe && c.GetLastError() != nil {
				c.logger.Debug("Connection has error in subscribe mode, exiting read loop")
				c.setConnected(false)
				return
			}

			// Set read deadline - much longer timeout in subscribe mode for hibernated connections
			timeout := 30 * time.Second
			if c.config.Subscribe {
				// In subscribe mode, use very long timeout (1 hour) to allow for hibernated connections
				timeout = 1 * time.Hour
			}
			conn.SetReadDeadline(time.Now().Add(timeout))

			messageType, data, err := conn.ReadMessage()
			if err != nil {
				c.logger.Debug("ReadMessage error", zap.Error(err))

				// Check if this is an expected/normal connection closure
				if websocket.IsCloseError(err,
					websocket.CloseNoStatusReceived, // 1005 - normal close from server after ORIGIN_END
					websocket.CloseNormalClosure,    // 1000 - normal closure
					websocket.CloseGoingAway) {      // 1001 - endpoint going away
					c.logger.Info("WebSocket connection closed normally", zap.Error(err))
				} else if strings.Contains(err.Error(), "use of closed network connection") {
					// This happens when we close the connection during shutdown - it's expected
					c.logger.Debug("Connection closed during shutdown", zap.Error(err))
				} else {
					// Any other error is unexpected
					//c.logger.Error("WebSocket read error", zap.Error(err))
				}
				c.setError(err)
				c.setConnected(false)

				// If this is a normal closure, close read queue immediately
				if websocket.IsCloseError(err,
					websocket.CloseNoStatusReceived,
					websocket.CloseNormalClosure,
					websocket.CloseGoingAway) {
					c.logger.Debug("Normal closure - closing read queue immediately")
					// Close the read queue to signal no more data
					select {
					case <-c.readQueue:
						// Already closed
					default:
						close(c.readQueue)
					}
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
				configMsgResp := "CONFIG="
				messageResp := "MESSAGE="
				abortResp := "ABORT="
				// Handle text messages for NEWPULLKEY, NEWPUSHKEY, REPLICAID
				// Example: "NEWPULLKEY=xxxxxxxxxxxxxxxxxxxxxx"
				strData := string(data)

				if len(data) >= len(abortResp) && strings.HasPrefix(strData, abortResp) {
					color.Red("❌ Server aborted connection: %s", strData[len(abortResp):])
					c.setConnected(false)
					message := strData[len(abortResp):]
					c.setError(fmt.Errorf("server aborted connection: %s", message))
				} else if (len(data) >= len(configMsgResp)) && strings.HasPrefix(strData, configMsgResp) {
					// CONFIG={JSON}
					jsonStr := strData[len(configMsgResp):]
					var configMsg map[string]interface{}
					err := json.Unmarshal([]byte(jsonStr), &configMsg)
					if err != nil {
						c.logger.Error("Failed to parse CONFIG JSON", zap.Error(err))
						continue
					}
					if configMsg["newPullKey"] != nil {
						c.NewPullKey = configMsg["newPullKey"].(string)
					}
					if configMsg["newPushKey"] != nil {
						c.NewPushKey = configMsg["newPushKey"].(string)
					}
					if configMsg["replicaID"] != nil {
						c.ReplicaID = configMsg["replicaID"].(string)
					}
					if configMsg["replicaPath"] != nil {
						c.ReplicaPath = configMsg["replicaPath"].(string)
					}
					if configMsg["committedVersionID"] != nil {
						c.Version = configMsg["committedVersionID"].(string)
					}
				} else if (len(data) >= len(messageResp)) && strings.HasPrefix(strData, messageResp) {
					fmt.Println(strData[len(messageResp):])
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
				// Don't mark as completed yet - let the C code process all remaining data first
				// The Read method will mark it as completed when it actually receives ORIGIN_END
			} else if msgType == "SQLRSYNC_NEWREPLICAVERSION" && c.config.Subscribe {
				// Handle new version notification in subscribe mode
				c.logger.Info("SQLRSYNC_NEWREPLICAVERSION (0x52) received - new version available!")
				select {
				case c.newVersionChan <- struct{}{}:
					c.logger.Debug("New version notification sent to channel")
				default:
					c.logger.Debug("New version channel already has pending notification")
				}
				// Don't queue this message for normal reading
				continue
			}

			// Handle progress tracking in read loop
			if c.config.ProgressCallback != nil {
				c.inspector.InspectForProgress(data, "IN (Server → Client)", func(event SyncProgressEvent) {
					c.handleProgressEvent(event)
				}, c.config.EnableTrafficInspection)
			}
			// Queue the data for reading
			select {
			case c.readQueue <- data:
				c.logger.Debug("Data queued for reading", zap.Int("bytes", len(data)))
			case <-c.ctx.Done():
				return
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

			// consider moving this to InspectorOutbound

			// Do this here so ORIGIN_BEGIN sends first
			if c.config.SendConfigCmd {
				conn.WriteMessage(websocket.BinaryMessage, []byte{SQLRSYNC_CONFIG})
				c.config.SendConfigCmd = false
			}
			if c.config.SendKeyRequest {
				conn.WriteMessage(websocket.BinaryMessage, []byte{SQLRSYNC_KEYREQUEST})
				c.config.SendKeyRequest = false
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
func (c *Client) GetVersion() string {
	return c.Version
}

// WaitForNewVersion blocks until a new version notification is received (0x52)
// Returns nil when a new version is available, or an error if the connection is lost
func (c *Client) WaitForNewVersion() error {
	if !c.config.Subscribe {
		return fmt.Errorf("subscribe mode not enabled")
	}

	c.logger.Info("Waiting for new version notification...")

	// Check if connection is still alive
	if !c.isConnected() {
		return fmt.Errorf("connection lost")
	}

	// Check if there's already a notification pending
	select {
	case <-c.newVersionChan:
		c.logger.Info("Found pending new version notification!")
		return nil
	default:
		// No pending notification, continue to blocking wait
	}

	c.logger.Debug("No pending notifications, blocking wait...")

	// Use a ticker to periodically check for context cancellation
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			c.logger.Debug("Context cancelled during new version wait")
			return fmt.Errorf("client context cancelled")
		case <-c.newVersionChan:
			c.logger.Info("New version notification received from blocking wait!")
			return nil
		case <-ticker.C:
			// Check if connection is still alive
			if !c.isConnected() {
				return fmt.Errorf("connection lost while waiting")
			}
			// Continue waiting
		}
	}
}

// ResetForNewSync prepares the client for a new sync operation while maintaining the connection
func (c *Client) ResetForNewSync() {
	c.setSyncCompleted(false)
	// Clear any pending data in the read queue
	for {
		select {
		case <-c.readQueue:
			// Drain the queue
		default:
			return
		}
	}
}
