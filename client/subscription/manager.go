package subscription

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// Message types for subscription communication
const (
	MsgTypeLatestVersion = "LATEST_VERSION"
	MsgTypePing          = "PING"
	MsgTypePong          = "PONG"
	MsgTypeSubscribe     = "SUBSCRIBE"
	MsgTypeSubscribed    = "SUBSCRIBED"
	MsgTypeUnsubscribe   = "UNSUBSCRIBE"
	MsgTypeError         = "ERROR"
)
const PING_INTERVAL = 1 * time.Hour

// Message represents a subscription control message
type Message struct {
	Type      string                 `json:"type"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Timestamp time.Time              `json:"timestamp"`
}

// ManagerConfig holds subscription manager configuration
type ManagerConfig struct {
	ServerURL             string
	ReplicaPath           string
	AccessKey             string
	ReplicaID             string
	WsID                  string // websocket ID for client identification
	ClientVersion         string // version of the client software
	Logger                *zap.Logger
	MaxReconnectAttempts  int           // Maximum number of reconnect attempts (0 = infinite)
	InitialReconnectDelay time.Duration // Initial delay before first reconnect
	MaxReconnectDelay     time.Duration // Maximum delay between reconnect attempts
}

// Manager handles WebSocket subscriptions for new version notifications
// with automatic reconnection using exponential backoff when connections are lost.
//
// The manager will automatically attempt to reconnect when WebSocket errors occur,
// with delays starting at InitialReconnectDelay and doubling until
// MaxReconnectDelay is reached. Reconnection attempts continue indefinitely unless
// MaxReconnectAttempts is set to a positive value.
type Manager struct {
	config    *ManagerConfig
	logger    *zap.Logger
	conn      *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.RWMutex
	connected bool

	// Reconnection state
	reconnectAttempts int
	lastConnectTime   time.Time
	reconnecting      bool

	// Event channels
	newVersionChan chan string
	errorChan      chan error
}

// NewManager creates a new subscription manager
func NewManager(config *ManagerConfig) *Manager {
	ctx, cancel := context.WithCancel(context.Background())

	// Set default reconnection parameters if not provided
	if config.MaxReconnectAttempts == 0 {
		config.MaxReconnectAttempts = -1 // Infinite reconnect attempts by default
	}
	if config.InitialReconnectDelay == 0 {
		config.InitialReconnectDelay = 3 * time.Second
	}
	if config.MaxReconnectDelay == 0 {
		config.MaxReconnectDelay = 300 * time.Second // 5 minutes max
	}

	return &Manager{
		config:         config,
		logger:         config.Logger,
		ctx:            ctx,
		cancel:         cancel,
		newVersionChan: make(chan string, 1),
		errorChan:      make(chan error, 1),
	}
}

// Connect establishes WebSocket connection for subscription with automatic reconnection.
// If the initial connection fails, it will retry according to the configured reconnection parameters.
// Once connected, the manager will automatically handle reconnection if the connection is lost.
func (m *Manager) Connect() error {
	return m.connectWithRetry(false)
}

// connectWithRetry handles connection with optional retry logic
func (m *Manager) connectWithRetry(isReconnect bool) error {
	m.mu.Lock()
	if isReconnect {
		m.reconnecting = true
	}
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.reconnecting = false
		m.mu.Unlock()
	}()

	var lastErr error
	currentDelay := m.config.InitialReconnectDelay

	for attempt := 0; m.config.MaxReconnectAttempts < 0 || attempt < m.config.MaxReconnectAttempts; attempt++ {
		// Update reconnect attempts counter
		m.mu.Lock()
		m.reconnectAttempts = attempt + 1
		m.mu.Unlock()

		// Wait for delay on retry attempts (but not the very first attempt)
		if attempt > 0 {
			if isReconnect {
				m.logger.Info("Waiting before reconnect attempt",
					zap.Duration("delay", currentDelay),
					zap.Int("attempt", attempt+1))
			} else {
				m.logger.Info("Waiting before connection retry",
					zap.Duration("delay", currentDelay),
					zap.Int("attempt", attempt+1))
			}

			select {
			case <-m.ctx.Done():
				return fmt.Errorf("connection cancelled during backoff")
			case <-time.After(currentDelay):
			}
		}

		if err := m.doConnect(); err != nil {
			lastErr = err
			// Calculate next delay with exponential backoff for the following attempt
			currentDelay = m.calculateNextDelay(currentDelay)
			if isReconnect {
				m.logger.Warn("Reconnection attempt failed",
					zap.Error(err),
					zap.Int("attempt", attempt+1),
					zap.Duration("next_retry_in", currentDelay))
			} else {
				m.logger.Warn("Connection attempt failed",
					zap.Error(err),
					zap.Int("attempt", attempt+1),
					zap.Duration("next_retry_in", currentDelay))
			}

			continue
		}

		// Connection successful
		m.mu.Lock()
		m.reconnectAttempts = 0
		m.lastConnectTime = time.Now()
		m.mu.Unlock()

		if isReconnect {
			m.logger.Info("Successfully reconnected to subscription service",
				zap.Int("attempts", attempt+1))
		} else {
			m.logger.Info("Successfully connected to subscription service")
		}
		return nil
	}

	return fmt.Errorf("failed to connect after %d attempts: %w",
		m.config.MaxReconnectAttempts, lastErr)
}

// doConnect performs the actual WebSocket connection
func (m *Manager) doConnect() error {
	m.logger.Info("Connecting to subscription service", zap.String("url", m.config.ServerURL))

	u, err := url.Parse(m.config.ServerURL)
	if err != nil {
		return fmt.Errorf("invalid server URL: %w", err)
	}

	// Add subscription endpoint
	u.Path = strings.TrimSuffix(u.Path, "/") + "/sapi/subscribe/" + m.config.ReplicaPath

	headers := http.Header{}
	headers.Set("Authorization", m.config.AccessKey)
	if m.config.ReplicaID != "" {
		headers.Set("X-ReplicaID", m.config.ReplicaID)
	}

	headers.Set("X-ClientVersion", m.config.ClientVersion)
	headers.Set("X-ClientID", m.config.WsID)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	m.logger.Debug("Dialing WebSocket", zap.String("url", u.String()))

	conn, response, err := dialer.DialContext(m.ctx, u.String(), headers)
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

			// Connect to remote server
			if strings.Contains(err.Error(), "key is not authorized") || strings.Contains(err.Error(), "404 Path not found") {
				if m.config.AccessKey == "" {
					key, err := PromptForKey(m.config.ServerURL, m.config.ReplicaPath, "PULL")
					if err != nil {
						return fmt.Errorf("manager failed to get key interactively: %w", err)
					}
					m.config.AccessKey = key
					return m.doConnect()
				} else {
					return fmt.Errorf("manager failed to connect to server: %w", err)
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
		errorMsg.WriteString("Failed to connect to subscription service")

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

	m.mu.Lock()
	m.conn = conn
	m.connected = true
	m.mu.Unlock()

	// Start message handling loops
	go m.readLoop()
	go m.pingLoop()

	// Send subscription message
	if err := m.sendMessage(Message{
		Type: MsgTypeSubscribe,
		Data: map[string]interface{}{
			"replicaID": m.config.ReplicaID,
		},
		Timestamp: time.Now(),
	}); err != nil {
		return fmt.Errorf("failed to send subscribe message: %w", err)
	}

	return nil
}

// WaitForNewVersionMsg blocks until a new version is available
func (m *Manager) WaitForNewVersionMsg() (string, error) {
	m.logger.Info("Waiting for a new version notification...")

	for {
		select {
		case <-m.ctx.Done():
			return "", fmt.Errorf("subscription cancelled")
		case err := <-m.errorChan:
			// Check if this is a reconnection failure
			if strings.Contains(err.Error(), "reconnection failed") {
				m.logger.Error("Reconnection failed permanently", zap.Error(err))
				return "", err
			}
			// For other errors, log and continue waiting (reconnection might be in progress)
			m.logger.Warn("Temporary subscription error", zap.Error(err))
			continue
		case version := <-m.newVersionChan:
			m.logger.Info("Latest Version message received", zap.String("version", version))
			return version, nil
		}
	}
}

// Close cleanly shuts down the subscription manager
func (m *Manager) Close() error {
	m.logger.Info("Closing subscription manager")

	// Cancel context to stop all operations
	m.cancel()

	m.mu.Lock()
	if m.conn != nil {
		// Send unsubscribe message (best effort)
		m.sendMessage(Message{
			Type:      MsgTypeUnsubscribe,
			Timestamp: time.Now(),
		})

		// Close connection gracefully
		m.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(5*time.Second))
		m.conn.Close()
		m.conn = nil
	}
	m.connected = false
	m.reconnecting = false
	m.mu.Unlock()

	return nil
}

// IsConnected returns current connection status
func (m *Manager) IsConnected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connected
}

// IsReconnecting returns whether the manager is currently attempting to reconnect
func (m *Manager) IsReconnecting() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.reconnecting
}

// GetConnectionStatus returns detailed connection status information
func (m *Manager) GetConnectionStatus() (connected bool, reconnecting bool, lastConnectTime time.Time, reconnectAttempts int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connected, m.reconnecting, m.lastConnectTime, m.reconnectAttempts
}

// calculateNextDelay calculates the next delay using exponential backoff
func (m *Manager) calculateNextDelay(currentDelay time.Duration) time.Duration {
	nextDelay := time.Duration(float64(currentDelay) * 2.0)
	if nextDelay > m.config.MaxReconnectDelay {
		return m.config.MaxReconnectDelay
	}
	return nextDelay
}

// sendMessage sends a message to the server
func (m *Manager) sendMessage(msg Message) error {
	m.mu.RLock()
	conn := m.conn
	m.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	var data []byte
	var err error

	if msg.Type == MsgTypePing {
		data = []byte("PING")
	} else if msg.Type == MsgTypePong {
		data = []byte("PONG")
	} else {
		data, err = json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("failed to marshal message: %w", err)
		}
	}

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, data)
}

// readLoop handles incoming messages
func (m *Manager) readLoop() {
	defer func() {
		m.mu.Lock()
		if m.conn != nil {
			m.conn.Close()
			m.conn = nil
		}
		m.connected = false
		m.mu.Unlock()
	}()

	for {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		m.mu.RLock()
		conn := m.conn
		m.mu.RUnlock()

		if conn == nil {
			return
		}

		conn.SetReadDeadline(time.Now().Add(PING_INTERVAL + 2*time.Minute)) // Longer than ping interval

		messageType, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				m.logger.Info("WebSocket connection closed normally")
				return
			}

			m.logger.Error("WebSocket read error", zap.Error(err))

			// Mark as disconnected
			m.mu.Lock()
			if m.conn != nil {
				m.conn.Close()
				m.conn = nil
			}
			m.connected = false
			wasReconnecting := m.reconnecting
			m.mu.Unlock()

			// Only attempt reconnection if we're not already reconnecting
			// and the context hasn't been cancelled
			select {
			case <-m.ctx.Done():
				return
			default:
				if !wasReconnecting {
					m.logger.Info("Attempting to reconnect after connection loss")
					go m.attemptReconnect()
				}
			}
			return
		}

		if messageType != websocket.TextMessage {
			continue
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			m.logger.Warn("Failed to unmarshal message", zap.Error(err))
			continue
		}

		m.handleMessage(msg)
	}
}

// attemptReconnect handles automatic reconnection logic
func (m *Manager) attemptReconnect() {
	fmt.Println("🔄 Connection lost. Attempting to reconnect...")

	if err := m.connectWithRetry(true); err != nil {
		m.logger.Error("Failed to reconnect to subscription service", zap.Error(err))
		fmt.Printf("❌ Reconnection failed: %v\n", err)
		// Send error to error channel for coordinator to handle
		select {
		case m.errorChan <- fmt.Errorf("reconnection failed: %w", err):
		default:
		}
	} else {
		fmt.Println("✅ Reconnected successfully! Continuing to watch for updates...")
	}
}

// handleMessage processes incoming subscription messages
func (m *Manager) handleMessage(msg Message) {
	// Only log debug for non-PONG messages
	if false && msg.Type != MsgTypePong {
		m.logger.Debug("Received message", zap.String("type", msg.Type))
	}

	switch msg.Type {
	case MsgTypeLatestVersion, MsgTypeSubscribed:
		latestVersion, ok := msg.Data["version"].(string)
		if !ok {
			actualValue := msg.Data["version"]
			m.logger.Error("Invalid LATEST_VERSION message format: version field is not a string",
				zap.Any("data", msg.Data),
				zap.Any("actualVersion", actualValue),
				zap.String("actualType", fmt.Sprintf("%T", actualValue)))
			latestVersion = "latest"
		}
		select {
		case m.newVersionChan <- latestVersion:
		default:
			// Channel full, version notification already pending
		}

	case MsgTypePing:
		// Respond to server ping
		m.sendMessage(Message{
			Type:      MsgTypePong,
			Timestamp: time.Now(),
		})

	case MsgTypePong:
		// Server responded to our ping - connection is healthy
		m.logger.Info("Received PONG - connection healthy")

	case MsgTypeError:
		errMsg, _ := msg.Data["message"].(string)
		err := fmt.Errorf("subscription error: %s", errMsg)
		select {
		case m.errorChan <- err:
		default:
		}
	default:
		m.logger.Debug("Unknown message type", zap.String("type", msg.Type))
	}
}

// pingLoop sends periodic ping messages
func (m *Manager) pingLoop() {
	ticker := time.NewTicker(PING_INTERVAL)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			// Check if we're connected before trying to ping
			m.mu.RLock()
			connected := m.connected && m.conn != nil
			m.mu.RUnlock()

			if !connected {
				// Connection lost, stop ping loop - readLoop will handle reconnection
				return
			}

			if err := m.sendMessage(Message{
				Type:      MsgTypePing,
				Timestamp: time.Now(),
			}); err != nil {
				m.logger.Error("Failed to send ping", zap.Error(err))
				// Don't send to error channel here - let readLoop handle the disconnection
				return
			}
		}
	}
}

// PromptForKey prompts the user for an admin key
func PromptForKey(serverURL string, remotePath string, keyType string) (string, error) {
	httpServer := strings.Replace(serverURL, "ws", "http", 1)
	fmt.Println("Replica not found when using unauthenticated access.  Try again using a key or check your spelling.")
	fmt.Println("   Get a key at " + httpServer + "/namespaces or " + httpServer + "/" + remotePath)
	fmt.Print("   Provide a key to " + keyType + ": ")

	reader := bufio.NewReader(os.Stdin)
	key, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read admin key: %w", err)
	}

	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("admin key cannot be empty")
	}

	return key, nil
}
