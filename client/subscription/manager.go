package subscription

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// Message types for subscription communication
const (
	MsgTypeNewVersion  = "NEW_VERSION"
	MsgTypePing        = "PING"
	MsgTypePong        = "PONG"
	MsgTypeSubscribe   = "SUBSCRIBE"
	MsgTypeUnsubscribe = "UNSUBSCRIBE"
	MsgTypeError       = "ERROR"
)

// Message represents a subscription control message
type Message struct {
	Type      string                 `json:"type"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Timestamp time.Time              `json:"timestamp"`
}

// Config holds subscription manager configuration
type Config struct {
	ServerURL   string
	ReplicaPath string
	AuthToken   string
	ReplicaID   string
	Logger      *zap.Logger
}

// Manager handles WebSocket subscriptions for new version notifications
type Manager struct {
	config    *Config
	logger    *zap.Logger
	conn      *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.RWMutex
	connected bool

	// Event channels
	newVersionChan chan string
	errorChan      chan error
}

// NewManager creates a new subscription manager
func NewManager(config *Config) *Manager {
	ctx, cancel := context.WithCancel(context.Background())

	return &Manager{
		config:         config,
		logger:         config.Logger,
		ctx:            ctx,
		cancel:         cancel,
		newVersionChan: make(chan string, 1),
		errorChan:      make(chan error, 1),
	}
}

// Connect establishes WebSocket connection for subscription
func (m *Manager) Connect() error {
	m.logger.Info("Connecting to subscription service", zap.String("url", m.config.ServerURL))

	u, err := url.Parse(m.config.ServerURL)
	if err != nil {
		return fmt.Errorf("invalid server URL: %w", err)
	}

	// Add subscription endpoint
	u.Path = strings.TrimSuffix(u.Path, "/") + "/sapi/subscribe/" + m.config.ReplicaPath

	headers := http.Header{}
	headers.Set("Authorization", m.config.AuthToken)
	if m.config.ReplicaID != "" {
		headers.Set("X-ReplicaID", m.config.ReplicaID)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	m.logger.Info("Dialing WebSocket", zap.String("url", u.String()))

	conn, _, err := dialer.DialContext(m.ctx, u.String(), headers)
	if err != nil {
		return fmt.Errorf("failed to connect to subscription service: %w", err)
	}

	m.mu.Lock()
	m.conn = conn
	m.connected = true
	m.mu.Unlock()

	// Start message handling
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

	m.logger.Info("Subscription connection established")
	return nil
}

// WaitForNewVersion blocks until a new version is available
func (m *Manager) WaitForNewVersion() (string, error) {
	m.logger.Info("Waiting for new version notification...")

	select {
	case <-m.ctx.Done():
		return "", fmt.Errorf("subscription cancelled")
	case err := <-m.errorChan:
		return "", err
	case version := <-m.newVersionChan:
		m.logger.Info("New version available", zap.String("version", version))
		return version, nil
	}
}

// Close cleanly shuts down the subscription manager
func (m *Manager) Close() error {
	m.logger.Info("Closing subscription manager")

	m.cancel()

	m.mu.Lock()
	if m.conn != nil {
		// Send unsubscribe message
		m.sendMessage(Message{
			Type:      MsgTypeUnsubscribe,
			Timestamp: time.Now(),
		})

		// Close connection
		m.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(5*time.Second))
		m.conn.Close()
		m.conn = nil
	}
	m.connected = false
	m.mu.Unlock()

	return nil
}

// IsConnected returns current connection status
func (m *Manager) IsConnected() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.connected
}

// sendMessage sends a message to the server
func (m *Manager) sendMessage(msg Message) error {
	m.mu.RLock()
	conn := m.conn
	m.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("not connected")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, data)
}

// readLoop handles incoming messages
func (m *Manager) readLoop() {
	defer func() {
		m.mu.Lock()
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

		conn.SetReadDeadline(time.Now().Add(70 * time.Second)) // Longer than ping interval

		messageType, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				m.logger.Info("WebSocket connection closed normally")
				return
			}
			m.logger.Error("WebSocket read error", zap.Error(err))
			select {
			case m.errorChan <- err:
			default:
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

// handleMessage processes incoming subscription messages
func (m *Manager) handleMessage(msg Message) {
	// Only log debug for non-PONG messages
	if msg.Type != MsgTypePong {
		m.logger.Debug("Received message", zap.String("type", msg.Type))
	}

	switch msg.Type {
	case MsgTypeNewVersion:
		version, ok := msg.Data["version"].(string)
		if !ok {
			version = "latest"
		}
		select {
		case m.newVersionChan <- version:
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
	ticker := time.NewTicker(60 * time.Second) // 1 minute ping interval
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if err := m.sendMessage(Message{
				Type:      MsgTypePing,
				Timestamp: time.Now(),
			}); err != nil {
				m.logger.Error("Failed to send ping", zap.Error(err))
				select {
				case m.errorChan <- err:
				default:
				}
				return
			}
		}
	}
}
