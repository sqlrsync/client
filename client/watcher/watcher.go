package watcher

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// WriteNotificationFunc is called when a write is detected (for analytics)
type WriteNotificationFunc func(path string, timestamp time.Time) error

// Config holds file watcher configuration
type Config struct {
	DatabasePath         string
	Logger               *zap.Logger
	WriteNotificationFunc WriteNotificationFunc // Optional: called on write detection
}

// Watcher monitors file changes for SQLite database and WAL files
type Watcher struct {
	config   *Config
	logger   *zap.Logger
	watcher  *fsnotify.Watcher
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.RWMutex
	changeCh chan time.Time
}

// NewWatcher creates a new file watcher
func NewWatcher(config *Config) (*Watcher, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	if config.Logger == nil {
		return nil, fmt.Errorf("logger cannot be nil")
	}

	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("failed to create file watcher: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	w := &Watcher{
		config:   config,
		logger:   config.Logger,
		watcher:  fsWatcher,
		ctx:      ctx,
		cancel:   cancel,
		changeCh: make(chan time.Time, 10),
	}

	return w, nil
}

// Start begins watching for file changes
func (w *Watcher) Start() error {
	// Watch the database file
	if err := w.watcher.Add(w.config.DatabasePath); err != nil {
		return fmt.Errorf("failed to watch database file: %w", err)
	}

	// Watch the WAL file if it exists
	walPath := w.config.DatabasePath + "-wal"
	if err := w.watcher.Add(walPath); err != nil {
		w.logger.Debug("WAL file not found or cannot be watched", zap.String("path", walPath))
	}

	// Start the event processing loop
	go w.eventLoop()

	w.logger.Info("File watcher started", zap.String("path", w.config.DatabasePath))
	return nil
}

// eventLoop processes file system events
func (w *Watcher) eventLoop() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}

			// Only care about Write and Create events
			if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
				// Check if it's the database or WAL file
				baseName := filepath.Base(event.Name)
				dbBaseName := filepath.Base(w.config.DatabasePath)
				walBaseName := dbBaseName + "-wal"

				if baseName == dbBaseName || baseName == walBaseName {
					changeTime := time.Now()
					w.logger.Debug("File change detected", zap.String("file", event.Name), zap.String("op", event.Op.String()))

					// Send write notification to server for analytics (optional, non-blocking)
					if w.config.WriteNotificationFunc != nil {
						go func() {
							if err := w.config.WriteNotificationFunc(event.Name, changeTime); err != nil {
								w.logger.Debug("Failed to send write notification to server", zap.Error(err))
							}
						}()
					}

					// Send change notification (non-blocking)
					select {
					case w.changeCh <- changeTime:
					default:
						// Channel full, change already pending
					}
				}
			}
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			w.logger.Error("File watcher error", zap.Error(err))
		}
	}
}

// WaitForChange blocks until a file change is detected or context is cancelled
func (w *Watcher) WaitForChange() (time.Time, error) {
	select {
	case <-w.ctx.Done():
		return time.Time{}, fmt.Errorf("watcher cancelled")
	case changeTime := <-w.changeCh:
		return changeTime, nil
	}
}

// Close stops the file watcher
func (w *Watcher) Close() error {
	w.cancel()
	return w.watcher.Close()
}
