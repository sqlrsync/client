package remote

import (
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// TrackedWaitGroup is a wrapper around sync.WaitGroup that tracks which goroutines are running
type TrackedWaitGroup struct {
	wg      sync.WaitGroup
	mu      sync.RWMutex
	running map[string]bool // Track which goroutines are running by name
	logger  *zap.Logger
}

// NewTrackedWaitGroup creates a new TrackedWaitGroup
func NewTrackedWaitGroup(logger *zap.Logger) *TrackedWaitGroup {
	return &TrackedWaitGroup{
		running: make(map[string]bool),
		logger:  logger,
	}
}

// Add increments the WaitGroup counter and tracks the goroutine by name
func (t *TrackedWaitGroup) Add(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.running[name] = true
	t.wg.Add(1)
	t.logger.Debug("Goroutine started", zap.String("name", name))
}

// Done decrements the WaitGroup counter and marks the goroutine as completed
func (t *TrackedWaitGroup) Done(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.running, name)
	t.wg.Done()
	t.logger.Debug("Goroutine completed", zap.String("name", name))
}

// Wait blocks until all goroutines have called Done
func (t *TrackedWaitGroup) Wait() {
	t.wg.Wait()
}

// WaitWithTimeout waits for all goroutines to complete with a timeout
// Returns true if all completed, false if timeout occurred
func (t *TrackedWaitGroup) WaitWithTimeout(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.logger.Debug("All goroutines completed successfully")
		return true
	case <-time.After(timeout):
		t.LogStillRunning()
		return false
	}
}

// GetRunning returns a list of goroutines that are still running
func (t *TrackedWaitGroup) GetRunning() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()

	running := make([]string, 0, len(t.running))
	for name := range t.running {
		running = append(running, name)
	}
	return running
}

// LogStillRunning logs which goroutines are still running
func (t *TrackedWaitGroup) LogStillRunning() {
	running := t.GetRunning()
	if len(running) == 0 {
		t.logger.Debug("No goroutines still running")
		return
	}

	t.logger.Warn(
		"Goroutines still running",
		zap.Int("count", len(running)),
		zap.Strings("names", running),
	)
}

// String returns a string representation of the running goroutines
func (t *TrackedWaitGroup) String() string {
	running := t.GetRunning()
	if len(running) == 0 {
		return "No goroutines running"
	}
	return fmt.Sprintf("%d goroutines running: %v", len(running), running)
}
