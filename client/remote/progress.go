package remote

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ProgressFormatter provides different output formats for progress events
type ProgressFormatter struct {
	config *ProgressConfig
}

// NewProgressFormatter creates a new progress formatter
func NewProgressFormatter(config *ProgressConfig) *ProgressFormatter {
	if config == nil {
		config = &ProgressConfig{
			Enabled:        true,
			Format:         FormatSimple,
			UpdateRate:     500 * time.Millisecond,
			ShowETA:        true,
			ShowBytes:      true,
			ShowPages:      true,
			PagesPerUpdate: 10,
		}
	}
	return &ProgressFormatter{config: config}
}

// FormatProgress formats a progress event according to the configured format
func (f *ProgressFormatter) FormatProgress(event SyncProgressEvent) string {
	if !f.config.Enabled {
		return ""
	}

	switch f.config.Format {
	case FormatSimple:
		return f.formatSimple(event)
	case FormatDetailed:
		return f.formatDetailed(event)
	case FormatJSON:
		return f.formatJSON(event)
	default:
		return f.formatSimple(event)
	}
}

// formatSimple creates a simple one-line progress display
func (f *ProgressFormatter) formatSimple(event SyncProgressEvent) string {
	if event.Progress == nil {
		return fmt.Sprintf("• %s", event.Message)
	}

	progress := event.Progress
	direction := "Syncing"
	if progress.Direction == DirectionPush {
		direction = "Pushing"
	} else if progress.Direction == DirectionPull {
		direction = "Pulling"
	}

	phase := f.getPhaseString(progress.Phase)

	switch event.Type {
	case EventSyncStart:
		return fmt.Sprintf("%s: Starting sync (%d pages, %s)",
			direction, progress.TotalPages, f.formatBytes(progress.TotalBytes))

	case EventNegotiationComplete:
		return fmt.Sprintf("%s: %s - ready to transfer", direction, phase)

	case EventSyncComplete:
		elapsed := time.Since(progress.StartTime)
		return fmt.Sprintf("%s: ✅ Complete - %d pages in %s",
			direction, progress.TotalPages, f.formatDuration(elapsed))

	default:
		// Active transfer progress
		if progress.TotalPages > 0 {
			var completedPages int
			if progress.Direction == DirectionPush {
				completedPages = progress.PagesSent
			} else {
				completedPages = progress.PagesReceived
			}

			progressBar := f.createProgressBar(progress.PercentComplete, 20)
			parts := []string{
				fmt.Sprintf("%s: %s %.1f%% (%d/%d pages)",
					direction, progressBar, progress.PercentComplete, completedPages, progress.TotalPages),
			}

			if f.config.ShowBytes {
				parts = append(parts, f.formatBytes(progress.BytesTransferred)+"/"+f.formatBytes(progress.TotalBytes))
			}

			if f.config.ShowETA && progress.EstimatedETA > 0 {
				parts = append(parts, "ETA: "+f.formatDuration(progress.EstimatedETA))
			}

			return strings.Join(parts, " • ")
		}

		return fmt.Sprintf("%s: %s", direction, event.Message)
	}
}

// formatDetailed creates a detailed multi-line progress display
func (f *ProgressFormatter) formatDetailed(event SyncProgressEvent) string {
	if event.Progress == nil {
		return fmt.Sprintf("• %s", event.Message)
	}

	progress := event.Progress
	direction := "Database Sync"
	if progress.Direction == DirectionPush {
		direction = "Push to Remote"
	} else if progress.Direction == DirectionPull {
		direction = "Pull from Remote"
	}

	phase := f.getPhaseString(progress.Phase)

	switch event.Type {
	case EventSyncStart:
		return fmt.Sprintf(`╭─ %s ─────────────────────────────────────╮
│ Starting: %d pages (%s)               │
│ Phase: %s                             │
╰──────────────────────────────────────────────────╯`,
			direction, progress.TotalPages, f.formatBytes(progress.TotalBytes), phase)

	case EventSyncComplete:
		elapsed := time.Since(progress.StartTime)
		avgSpeed := float64(progress.TotalPages) / elapsed.Seconds()
		return fmt.Sprintf(`╭─ %s Complete ───────────────────────────╮
│ ✅ Successfully synced %d pages          │
│ Time: %s                                 │
│ Average speed: %.1f pages/sec            │
╰──────────────────────────────────────────────────╯`,
			direction, progress.TotalPages, f.formatDuration(elapsed), avgSpeed)

	default:
		// Active transfer progress
		if progress.TotalPages > 0 {
			var completedPages int
			if progress.Direction == DirectionPush {
				completedPages = progress.PagesSent
			} else {
				completedPages = progress.PagesReceived
			}

			progressBar := f.createProgressBar(progress.PercentComplete, 40)

			lines := []string{
				fmt.Sprintf("╭─ %s Progress ───────────────────────────╮", direction),
				fmt.Sprintf("│ %s %.1f%% │", progressBar, progress.PercentComplete),
				"│                                              │",
			}

			if f.config.ShowPages {
				lines = append(lines, fmt.Sprintf("│ Pages: %d/%d (sent: %d, confirmed: %d)   │",
					completedPages, progress.TotalPages, progress.PagesSent, progress.PagesConfirmed))
			}

			if f.config.ShowBytes {
				lines = append(lines, fmt.Sprintf("│ Data: %s/%s                       │",
					f.formatBytes(progress.BytesTransferred), f.formatBytes(progress.TotalBytes)))
			}

			if progress.PagesPerSecond > 0 {
				lines = append(lines, fmt.Sprintf("│ Speed: %.1f pages/sec                    │", progress.PagesPerSecond))
			}

			if f.config.ShowETA && progress.EstimatedETA > 0 {
				lines = append(lines, fmt.Sprintf("│ ETA: %s                                  │", f.formatDuration(progress.EstimatedETA)))
			}

			lines = append(lines, "╰──────────────────────────────────────────────────╯")
			return strings.Join(lines, "\n")
		}

		return fmt.Sprintf("• %s: %s", direction, event.Message)
	}
}

// formatJSON creates a JSON representation of the progress
func (f *ProgressFormatter) formatJSON(event SyncProgressEvent) string {
	data := map[string]interface{}{
		"timestamp": time.Now().Unix(),
		"event":     f.getEventTypeString(event.Type),
		"message":   event.Message,
	}

	if event.Progress != nil {
		progress := event.Progress

		var completedPages int
		if progress.Direction == DirectionPush {
			completedPages = progress.PagesSent
		} else {
			completedPages = progress.PagesReceived
		}

		data["progress"] = map[string]interface{}{
			"phase":            f.getPhaseString(progress.Phase),
			"direction":        f.getDirectionString(progress.Direction),
			"percent_complete": progress.PercentComplete,
			"pages": map[string]interface{}{
				"completed": completedPages,
				"sent":      progress.PagesSent,
				"received":  progress.PagesReceived,
				"confirmed": progress.PagesConfirmed,
				"total":     progress.TotalPages,
			},
			"bytes": map[string]interface{}{
				"transferred": progress.BytesTransferred,
				"total":       progress.TotalBytes,
				"page_size":   progress.PageSize,
			},
			"timing": map[string]interface{}{
				"start_time":      progress.StartTime.Unix(),
				"last_update":     progress.LastUpdate.Unix(),
				"elapsed_seconds": time.Since(progress.StartTime).Seconds(),
				"eta_seconds":     progress.EstimatedETA.Seconds(),
				"pages_per_sec":   progress.PagesPerSecond,
			},
		}
	}

	jsonData, _ := json.Marshal(data)
	return string(jsonData)
}

// Helper methods

func (f *ProgressFormatter) createProgressBar(percent float64, width int) string {
	filled := int(percent / 100.0 * float64(width))
	if filled > width {
		filled = width
	}

	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return fmt.Sprintf("[%s]", bar)
}

func (f *ProgressFormatter) formatBytes(bytes int64) string {
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(bytes)/1024)
	} else if bytes < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1024*1024))
	} else {
		return fmt.Sprintf("%.1f GB", float64(bytes)/(1024*1024*1024))
	}
}

func (f *ProgressFormatter) formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	} else if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	} else if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	} else {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func (f *ProgressFormatter) getPhaseString(phase ProgressPhase) string {
	switch phase {
	case PhaseInitializing:
		return "Initializing"
	case PhaseNegotiating:
		return "Negotiating"
	case PhaseTransferring:
		return "Transferring"
	case PhaseCompleting:
		return "Completing"
	case PhaseCompleted:
		return "Completed"
	default:
		return "Unknown"
	}
}

func (f *ProgressFormatter) getDirectionString(direction SyncDirection) string {
	switch direction {
	case DirectionPush:
		return "push"
	case DirectionPull:
		return "pull"
	default:
		return "unknown"
	}
}

func (f *ProgressFormatter) getEventTypeString(eventType ProgressEventType) string {
	switch eventType {
	case EventSyncStart:
		return "sync_start"
	case EventNegotiationComplete:
		return "negotiation_complete"
	case EventPageSent:
		return "page_sent"
	case EventPageReceived:
		return "page_received"
	case EventPageConfirmed:
		return "page_confirmed"
	case EventSyncComplete:
		return "sync_complete"
	case EventError:
		return "error"
	default:
		return "unknown"
	}
}

// DefaultProgressCallback provides a simple callback that prints to stdout
func DefaultProgressCallback(format ProgressFormat) ProgressCallback {
	formatter := NewProgressFormatter(&ProgressConfig{
		Enabled:        true,
		Format:         format,
		UpdateRate:     500 * time.Millisecond,
		ShowETA:        true,
		ShowBytes:      true,
		ShowPages:      true,
		PagesPerUpdate: 10,
	})

	return func(event SyncProgressEvent) {
		output := formatter.FormatProgress(event)
		if output != "" {
			// For detailed format, clear previous lines
			if format == FormatDetailed && event.Type != EventSyncStart && event.Type != EventSyncComplete {
				// Simple clear - in real implementation might want to use terminal escape codes
				fmt.Print("\r")
			}

			fmt.Println(output)
		}
	}
}
