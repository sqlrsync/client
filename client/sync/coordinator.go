package sync

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/fatih/color"
	"github.com/sqlrsync/sqlrsync.com/auth"
	"github.com/sqlrsync/sqlrsync.com/bridge"
	"github.com/sqlrsync/sqlrsync.com/remote"
	"github.com/sqlrsync/sqlrsync.com/subscription"
	"github.com/sqlrsync/sqlrsync.com/watcher"
)

// Operation represents a sync operation type
type Operation int

const (
	OperationPull Operation = iota
	OperationPush
	OperationSubscribe
	OperationPushSubscribe
	OperationLocalSync
)

// CoordinatorConfig holds sync coordinator configuration
type CoordinatorConfig struct {
	ServerURL         string
	ProvidedAuthToken string // Explicitly provided auth token
	ProvidedPullKey   string // Explicitly provided pull key
	ProvidedPushKey   string // Explicitly provided push key
	ProvidedReplicaID string // Explicitly provided replica ID
	LocalPath         string
	RemotePath        string
	ReplicaPath       string // For LOCAL TO LOCAL sync
	Version           string
	Operation         Operation
	SetVisibility     int
	CommitMessage     []byte
	DryRun            bool
	Logger            *zap.Logger
	Verbose           bool
	Subscribing       bool
	WaitIdle          string
	MaxInterval       string
	MinInterval       string
	AutoMerge         bool
}

// Coordinator manages sync operations and subscriptions
type Coordinator struct {
	config        *CoordinatorConfig
	logger        *zap.Logger
	authResolver  *auth.Resolver
	subManager    *subscription.Manager
	remoteClient  *remote.Client // For PUSH subscription mode
	ctx           context.Context
	cancel        context.CancelFunc
	sessionStarted bool // Flag to track if we've sent initial SQLRSYNC_CHANGED for this session
	lastChangedSent time.Time // Track when we last sent SQLRSYNC_CHANGED
	resendTimer *time.Timer // Timer for resending SQLRSYNC_CHANGED 10s before expiration
}

// NewCoordinator creates a new sync coordinator
func NewCoordinator(config *CoordinatorConfig) *Coordinator {
	ctx, cancel := context.WithCancel(context.Background())

	return &Coordinator{
		config:       config,
		logger:       config.Logger,
		authResolver: auth.NewResolver(config.Logger),
		ctx:          ctx,
		cancel:       cancel,
	}
}

// Execute runs the sync operation
func (c *Coordinator) Execute() error {
	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		c.cancel()
		// Force exit after 2 seconds if graceful shutdown fails
		go func() {
			time.Sleep(2 * time.Second)
			fmt.Println("Force exiting...")
			os.Exit(0)
		}()
	}()

	switch c.config.Operation {
	case OperationPull:
		return c.executePull(false)
	case OperationPush:
		return c.executePush()
	case OperationSubscribe:
		return c.executeSubscribe()
	case OperationPushSubscribe:
		return c.executePushSubscribe()
	case OperationLocalSync:
		return c.executeLocalSync()
	default:
		return fmt.Errorf("unknown operation")
	}
}

// displayDryRunInfo displays dry run information for different operations
func (c *Coordinator) displayDryRunInfo(operation string, authResult *auth.ResolveResult, absLocalPath, serverURL, remotePath, localHostname string) {
	fmt.Println("SQLRsync Dry Run:")

	switch operation {
	case "push":
		fmt.Printf(" - Mode:           %s the LOCAL ORIGIN file up to the REMOTE REPLICA\n", color.YellowString("PUSHing"))
		fmt.Printf(" - LOCAL ORIGIN:   %s\n", color.GreenString(absLocalPath))
		if remotePath == "" {
			fmt.Println(" - REMOTE REPLICA: " + color.YellowString("(None - the server will assign a path using this hostname)"))
			fmt.Printf("   - Hostname:     %s\n", color.GreenString(localHostname))
		} else {
			fmt.Printf(" - REMOTE REPLICA: %s\n", color.GreenString(remotePath))
		}
	case "pull":
		fmt.Printf(" - Mode:           %s the REMOTE ORIGIN file down to LOCAL REPLICA\n", color.YellowString("PULLing"))
		fmt.Printf(" - REMOTE ORIGIN:  %s\n", color.GreenString(remotePath))
		fmt.Printf(" - LOCAL REPLICA:  %s\n", color.GreenString(absLocalPath))
	case "subscribe":
		fmt.Printf(" - Mode:           %s to REMOTE ORIGIN to PULL down current and future updates\n", color.YellowString("SUBSCRIBing"))
		fmt.Printf(" - REMOTE ORIGIN:  %s\n", color.GreenString(remotePath))
		fmt.Printf(" - LOCAL REPLICA:  %s\n", color.GreenString(absLocalPath))
	case "local":
		fmt.Printf(" - Mode:           %s between two databases\n", color.YellowString("LOCAL ONLY"))
	}

	if operation != "local" {
		fmt.Printf(" - Server:         %s\n", color.GreenString(serverURL))
		if authResult.AccessToken != "" {
			fmt.Printf(" - Access Token:   %s\n", color.GreenString(authResult.AccessToken))
		} else {
			fmt.Printf(" - Access Token:   %s\n", color.YellowString("(none)"))
		}

		if operation == "push" {
			switch c.config.SetVisibility {
			case 0:
				fmt.Println(" - Visibility:     " + color.YellowString("PRIVATE") + " (only accessible with access key)")
			case 1:
				fmt.Println(" - Visibility:     " + color.YellowString("UNLISTED") + " (anyone with the link can access)")
			case 2:
				fmt.Println(" - Visibility:     " + color.GreenString("PUBLIC") + " (anyone can access)")
			}
		}

		if c.authResolver.CheckNeedsDashFile(c.config.LocalPath, remotePath) {
			fmt.Println(" - A shareable config (the -sqlrsync file) " + color.GreenString("WILL BE") + " created for future PULLs and SUBSCRIBEs")
		} else {
			fmt.Println(" - A shareable config (the -sqlrsync file) will " + color.RedString("NOT") + " be created")
		}
	} else {
		// For local sync, show the replica path
		if c.config.ReplicaPath != "" {
			absReplicaPath, _ := filepath.Abs(c.config.ReplicaPath)
			fmt.Printf(" - LOCAL ORIGIN:   %s\n", color.GreenString(absLocalPath))
			fmt.Printf(" - LOCAL REPLICA:  %s\n", color.GreenString(absReplicaPath))
		}
	}
	fmt.Println("\nAfter running this command, REPLICA will become a copy of ORIGIN at the moment the command begins.")
}

// resolveAuth resolves authentication for the given operation
func (c *Coordinator) resolveAuth(operation string) (*auth.ResolveResult, error) {
	req := &auth.ResolveRequest{
		LocalPath:         c.config.LocalPath,
		RemotePath:        c.config.RemotePath,
		ServerURL:         c.config.ServerURL,
		ProvidedPullKey:   c.config.ProvidedPullKey,
		ProvidedPushKey:   c.config.ProvidedPushKey,
		ProvidedReplicaID: c.config.ProvidedReplicaID,
		Operation:         operation,
		Logger:            c.logger,
	}

	// Try explicit auth token first
	if c.config.ProvidedAuthToken != "" {
		return &auth.ResolveResult{
			AccessToken: c.config.ProvidedAuthToken,
			ReplicaID:   c.config.ProvidedReplicaID,
			ServerURL:   c.config.ServerURL,
			RemotePath:  c.config.RemotePath,
			LocalPath:   c.config.LocalPath,
		}, nil
	}

	result, err := c.authResolver.Resolve(req)
	if err != nil {
		return nil, err
	}

	// If prompting is needed for push operations
	if result.ShouldPrompt && operation == "push" {
		token, err := c.authResolver.PromptForAdminKey(c.config.ServerURL)
		if err != nil {
			return nil, err
		}
		result.AccessToken = token
		result.ShouldPrompt = false
	}

	return result, nil
}

// executeSubscribe runs pull sync with subscription for new versions
func (c *Coordinator) executeSubscribe() error {
	fmt.Println("📡 Subscribe mode enabled - will watch for new versions...")
	fmt.Println("   Press Ctrl+C to stop watching...")

	// Resolve authentication for subscription
	authResult, err := c.resolveAuth("subscribe")
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	// Check for dry run mode
	if c.config.DryRun {
		absLocalPath, err := filepath.Abs(c.config.LocalPath)
		if err != nil {
			return fmt.Errorf("failed to get absolute path: %w", err)
		}
		localHostname, _ := os.Hostname()

		serverURL := authResult.ServerURL
		if c.config.ServerURL != "" && c.config.ServerURL != "wss://sqlrsync.com" {
			serverURL = c.config.ServerURL
		}

		remotePath := authResult.RemotePath
		if c.config.RemotePath != "" {
			remotePath = c.config.RemotePath
		}

		c.displayDryRunInfo("subscribe", authResult, absLocalPath, serverURL, remotePath, localHostname)
		return nil
	}

	// Create subscription manager with reconnection configuration
	c.subManager = subscription.NewManager(&subscription.Config{
		ServerURL:             authResult.ServerURL,
		ReplicaPath:           authResult.RemotePath,
		AuthToken:             authResult.AccessToken,
		ReplicaID:             authResult.ReplicaID,
		Logger:                c.logger.Named("subscription"),
		MaxReconnectAttempts:  20,              // Infinite reconnect attempts
		InitialReconnectDelay: 5 * time.Second, // Start with 5 seconds delay
		MaxReconnectDelay:     5 * time.Minute, // Cap at 5 minutes
	})

	c.logger.Info("Starting subscription service", zap.String("server", authResult.ServerURL))

	// Connect to subscription service
	if err := c.subManager.Connect(); err != nil {
		return fmt.Errorf("failed to connect to subscription service2: %w", err)
	}
	defer c.subManager.Close()

	syncCount := 0
	for {
		syncCount++
		fmt.Printf("🔄 Starting sync...\n")

		// Perform pull sync
		if err := c.executePull(true); err != nil {
			c.logger.Error("Sync failed", zap.Error(err), zap.Int("syncCount", syncCount))
			return fmt.Errorf("sync #%d failed: %w", syncCount, err)
		}

		fmt.Printf("✅ Sync complete. Waiting for new version...\n")

		// Wait for new version or shutdown
		select {
		case <-c.ctx.Done():
			fmt.Println("Subscription stopped by user.")
			return nil
		default:
		}

		// Wait for new version notification
		var version string
		for {
			version, err = c.subManager.WaitForNewVersionMsg()
			if err != nil {
				// Check if this is a cancellation (graceful shutdown)
				if strings.Contains(err.Error(), "cancelled") {
					fmt.Println("Subscription stopped by user.")
					return nil
				}

				// Check if this is a permanent reconnection failure
				if strings.Contains(err.Error(), "reconnection failed") {
					fmt.Printf("❌ Failed to maintain connection to subscription service: %v\n", err)
					fmt.Println("   Please check your network connection and try again later.")
					return fmt.Errorf("subscription connection lost: %w", err)
				}

				c.logger.Error("Subscription error", zap.Error(err))
				return fmt.Errorf("subscription error: %w", err)
			}
			if c.config.Version == version {
				fmt.Printf("ℹ️  Already at version %s, waiting for next update...\n", version)
				continue
			} else {
				break
			}
		}

		fmt.Printf("🔄 New version %s announced!\n", version)
		// Update version for next sync
		if version != "latest" {
			c.config.Version = version
		}

	}
}

// executePull performs a single pull sync operation
func (c *Coordinator) executePull(isSubscription bool) error {
	// Resolve authentication
	authResult, err := c.resolveAuth("pull")
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	version := c.config.Version
	if version == "" {
		version = "latest"
	}

	// Use resolved values, with config overrides
	serverURL := authResult.ServerURL
	// Only override if user explicitly provided a different server (not just using the default)
	if c.config.ServerURL != "" && c.config.ServerURL != "wss://sqlrsync.com" {
		serverURL = c.config.ServerURL
	}

	remotePath := authResult.RemotePath
	if c.config.RemotePath != "" {
		remotePath = c.config.RemotePath
	}

	// Get absolute path and hostname for dry run display
	absLocalPath, err := filepath.Abs(c.config.LocalPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}
	localHostname, _ := os.Hostname()

	if c.config.DryRun {
		c.displayDryRunInfo("pull", authResult, absLocalPath, serverURL, remotePath, localHostname)
		return nil
	}

	if !isSubscription {
		fmt.Printf("PULLing down from %s/%s@%s ...\n", serverURL, remotePath, version)
	}

	// Create remote client for WebSocket transport
	remoteClient, err := remote.New(&remote.Config{
		ServerURL:               serverURL + "/sapi/pull/" + remotePath,
		AuthToken:               authResult.AccessToken,
		ReplicaID:               authResult.ReplicaID,
		Timeout:                 8000,
		PingPong:                false, // No ping/pong needed for single sync
		Logger:                  c.logger.Named("remote"),
		Subscribe:               false, // Subscription handled separately
		EnableTrafficInspection: c.config.Verbose,
		InspectionDepth:         5,
		Version:                 version,
		SendConfigCmd:           true,
		SendKeyRequest:          c.authResolver.CheckNeedsDashFile(c.config.LocalPath, remotePath),
		//ProgressCallback:        remote.DefaultProgressCallback(remote.FormatSimple),
		ProgressCallback: nil,
		ProgressConfig: &remote.ProgressConfig{
			Enabled:        true,
			Format:         remote.FormatSimple,
			UpdateRate:     500 * time.Millisecond,
			ShowETA:        true,
			ShowBytes:      true,
			ShowPages:      true,
			PagesPerUpdate: 10,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create remote client: %w", err)
	}
	defer remoteClient.Close()

	// Connect to remote server
	if err := remoteClient.Connect(); err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}

	// Create local client for SQLite operations
	localClient, err := bridge.New(&bridge.BridgeConfig{
		DatabasePath:             c.config.LocalPath,
		DryRun:                   c.config.DryRun,
		Logger:                   c.logger.Named("local"),
		EnableSQLiteRsyncLogging: c.config.Verbose,
	})
	if err != nil {
		return fmt.Errorf("failed to create local client: %w", err)
	}
	defer localClient.Close()

	// Perform the sync
	if err := c.performPullSync(localClient, remoteClient); err != nil {
		return fmt.Errorf("pull synchronization failed: %w", err)
	}
	c.config.Version = remoteClient.GetVersion()
	// Save pull result if needed
	if remoteClient.GetNewPullKey() != "" && c.authResolver.CheckNeedsDashFile(c.config.LocalPath, remotePath) {
		if err := c.authResolver.SavePullResult(
			c.config.LocalPath,
			serverURL,
			remoteClient.GetReplicaPath(),
			remoteClient.GetReplicaID(),
			remoteClient.GetNewPullKey(),
		); err != nil {
			c.logger.Warn("Failed to save pull result", zap.Error(err))
		} else {
			fmt.Println("🔑 Shareable config file created for future pulls")
		}
	}
	if !isSubscription {
		c.logger.Info("Pull synchronization completed successfully")
	}
	return nil
}

// executePush performs a push sync operation
func (c *Coordinator) executePush() error {
	// Validate that database file exists
	if _, err := os.Stat(c.config.LocalPath); os.IsNotExist(err) {
		return fmt.Errorf("database file does not exist: %s", c.config.LocalPath)
	}

	// Resolve authentication
	authResult, err := c.resolveAuth("push")
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	// Use resolved values, with config overrides
	serverURL := authResult.ServerURL
	// Only override if user explicitly provided a different server (not just using the default)
	if c.config.ServerURL != "" && c.config.ServerURL != "wss://sqlrsync.com" {
		serverURL = c.config.ServerURL
	}

	remotePath := authResult.RemotePath
	if c.config.RemotePath != "" {
		remotePath = c.config.RemotePath
	}

	// Create local client for SQLite operations
	localClient, err := bridge.New(&bridge.BridgeConfig{
		DatabasePath:             c.config.LocalPath,
		DryRun:                   c.config.DryRun,
		Logger:                   c.logger.Named("local"),
		EnableSQLiteRsyncLogging: c.config.Verbose,
	})
	if err != nil {
		return fmt.Errorf("failed to create local client: %w", err)
	}
	defer localClient.Close()

	// Get absolute path for the local database
	absLocalPath, err := filepath.Abs(c.config.LocalPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	localHostname, _ := os.Hostname()

	if c.config.DryRun {
		c.displayDryRunInfo("push", authResult, absLocalPath, serverURL, remotePath, localHostname)
		return nil
	}

	fmt.Printf("PUSHing up to %s/%s ...\n", serverURL, remotePath)

	// Create remote client for WebSocket transport
	remoteClient, err := remote.New(&remote.Config{
		ServerURL:               serverURL + "/sapi/push/" + remotePath,
		PingPong:                true,
		Timeout:                 15000,
		AuthToken:               authResult.AccessToken,
		Logger:                  c.logger.Named("remote"),
		EnableTrafficInspection: c.config.Verbose,
		LocalHostname:           localHostname,
		LocalAbsolutePath:       absLocalPath,
		InspectionDepth:         5,
		SendKeyRequest:          c.authResolver.CheckNeedsDashFile(c.config.LocalPath, remotePath),
		SendConfigCmd:           true,
		SetVisibility:           c.config.SetVisibility,
		CommitMessage:           c.config.CommitMessage,
		ProgressCallback:        nil, //remote.DefaultProgressCallback(remote.FormatSimple),
		ProgressConfig: &remote.ProgressConfig{
			Enabled:        true,
			Format:         remote.FormatSimple,
			UpdateRate:     500 * time.Millisecond,
			ShowETA:        true,
			ShowBytes:      true,
			ShowPages:      true,
			PagesPerUpdate: 10,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create remote client: %w", err)
	}
	defer remoteClient.Close()

	// Connect to remote server
	if err := remoteClient.Connect(); err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}

	// Perform the sync
	if err := c.performPushSync(localClient, remoteClient); err != nil {
		// Check if this was a version conflict and auto-merge is enabled
		if remoteClient.HasVersionConflict() && c.config.AutoMerge {
			latestVersion := remoteClient.GetLatestVersion()
			fmt.Printf("🔄 Auto-merge enabled - attempting to merge with server version %s...\n", latestVersion)

			if mergeErr := c.executeAutoMerge(localClient, remoteClient, latestVersion); mergeErr != nil {
				return fmt.Errorf("auto-merge failed: %w", mergeErr)
			}

			// Auto-merge succeeded, retry the push
			fmt.Println("✅ Auto-merge successful - retrying PUSH...")
			remoteClient.ResetVersionConflict()

			if err := c.performPushSync(localClient, remoteClient); err != nil {
				return fmt.Errorf("push after auto-merge failed: %w", err)
			}
		} else {
			return fmt.Errorf("push synchronization failed: %w", err)
		}
	}

	// Save push result if we got new keys
	if remoteClient.GetNewPushKey() != "" {
		if err := c.authResolver.SavePushResult(
			absLocalPath,
			serverURL,
			remoteClient.GetReplicaPath(),
			remoteClient.GetReplicaID(),
			remoteClient.GetNewPushKey(),
		); err != nil {
			c.logger.Warn("Failed to save push result", zap.Error(err))
		} else {
			fmt.Println("🔑 A new PUSH access key was stored at ~/.config/sqlrsync/ for ")
			fmt.Println("   revokable permission to push updates in the future.  Just")
			fmt.Println("   use `sqlrsync " + absLocalPath + "` to push again.")
		}
	}

	// Create -sqlrsync file for sharing if needed
	if c.authResolver.CheckNeedsDashFile(c.config.LocalPath, remotePath) && remoteClient.GetNewPullKey() != "" {
		if err := c.authResolver.SavePullResult(
			c.config.LocalPath,
			serverURL,
			remoteClient.GetReplicaPath(),
			remoteClient.GetReplicaID(),
			remoteClient.GetNewPullKey(),
		); err != nil {
			c.logger.Warn("Failed to create shareable config file", zap.Error(err))
		} else {
			fmt.Println("🔑 A new PULL access key was created: " + c.config.LocalPath + "-sqlrsync")
			fmt.Println("   Share this file to allow others to download or subscribe")
			fmt.Println("   to this database.")
		}
	}

	c.logger.Info("Push synchronization completed successfully")
	return nil
}

// performPullSync executes the pull synchronization
func (c *Coordinator) performPullSync(localClient *bridge.BridgeClient, remoteClient *remote.Client) error {
	// Create I/O bridge between remote and local clients
	readFunc := func(buffer []byte) (int, error) {
		return remoteClient.Read(buffer)
	}

	writeFunc := func(data []byte) error {
		return remoteClient.Write(data)
	}

	/*
		progress := remoteClient.GetProgress()
		if progress != nil {
			fmt.Printf("Current progress: %.1f%% (%d/%d pages)\n",
				progress.PercentComplete, progress.PagesSent, progress.TotalPages)
		}*/

	// Run the replica sync through the bridge
	return localClient.RunPullSync(readFunc, writeFunc)
}

// performPushSync executes the push synchronization
func (c *Coordinator) performPushSync(localClient *bridge.BridgeClient, remoteClient *remote.Client) error {
	// Create I/O bridge between local and remote clients
	readFunc := func(buffer []byte) (int, error) {
		return remoteClient.Read(buffer)
	}

	writeFunc := func(data []byte) error {
		return remoteClient.Write(data)
	}
	/*
		progress := remoteClient.GetProgress()
		if progress != nil {
			fmt.Printf("Current progress: %.1f%% (%d/%d pages)\n",
				progress.PercentComplete, progress.PagesSent, progress.TotalPages)
		}*/

	// Run the origin sync through the bridge
	return localClient.RunPushSync(readFunc, writeFunc)
}

// executeLocalSync performs a direct local-to-local sync operation
func (c *Coordinator) executeLocalSync() error {
	// Validate that both database files exist
	if _, err := os.Stat(c.config.LocalPath); os.IsNotExist(err) {
		return fmt.Errorf("origin database file does not exist: %s", c.config.LocalPath)
	}

	// For replica, it's okay if it doesn't exist - it will be created
	absOriginPath, err := filepath.Abs(c.config.LocalPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path for origin: %w", err)
	}

	absReplicaPath, err := filepath.Abs(c.config.ReplicaPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path for replica: %w", err)
	}

	if c.config.DryRun {
		c.displayDryRunInfo("local", nil, absOriginPath, "", "", "")
		return nil
	}

	fmt.Printf("Syncing LOCAL to LOCAL: %s → %s\n", absOriginPath, absReplicaPath)

	// Create local client for SQLite operations
	localClient, err := bridge.New(&bridge.BridgeConfig{
		DatabasePath:             absOriginPath,
		DryRun:                   c.config.DryRun,
		Logger:                   c.logger.Named("local"),
		EnableSQLiteRsyncLogging: c.config.Verbose,
	})
	if err != nil {
		return fmt.Errorf("failed to create local client: %w", err)
	}
	defer localClient.Close()

	// Perform direct sync
	if err := localClient.RunDirectSync(absReplicaPath); err != nil {
		return fmt.Errorf("local-to-local synchronization failed: %w", err)
	}

	c.logger.Info("Local-to-local synchronization completed successfully")
	fmt.Println("✅ Local sync completed")
	return nil
}

// executePushSubscribe performs an initial push then watches for file changes to auto-push
func (c *Coordinator) executePushSubscribe() error {
	fmt.Println("📡 PUSH Subscribe mode enabled - will push on file changes...")
	fmt.Println("   Press Ctrl+C to stop watching...")

	// Parse duration strings
	var waitIdleDuration, maxIntervalDuration, minIntervalDuration time.Duration
	var err error

	if c.config.WaitIdle == "" {
		return fmt.Errorf("--waitIdle is required for PUSH subscription mode")
	}

	waitIdleDuration, err = ParseDuration(c.config.WaitIdle)
	if err != nil {
		return fmt.Errorf("invalid --waitIdle: %w", err)
	}

	if err := ValidateWaitIdle(waitIdleDuration); err != nil {
		return fmt.Errorf("invalid --waitIdle: %w", err)
	}

	if c.config.MaxInterval != "" {
		maxIntervalDuration, err = ParseDuration(c.config.MaxInterval)
		if err != nil {
			return fmt.Errorf("invalid --maxInterval: %w", err)
		}
	}

	if c.config.MinInterval != "" {
		minIntervalDuration, err = ParseDuration(c.config.MinInterval)
		if err != nil {
			return fmt.Errorf("invalid --minInterval: %w", err)
		}
	} else if c.config.MaxInterval != "" {
		// Default minInterval to half of maxInterval
		minIntervalDuration = maxIntervalDuration / 2
	}

	// Perform initial PUSH
	fmt.Println("🔄 Performing initial PUSH...")
	if err := c.executePush(); err != nil {
		return fmt.Errorf("initial PUSH failed: %w", err)
	}

	lastPushTime := time.Now()
	fmt.Println("✅ Initial PUSH complete")

	// Check if we should continue with PUSH subscribe based on key type
	// The server will tell us via CONFIG what type of key we're using

	// Set up persistent remote client connection for sending CHANGED messages
	fmt.Println("📡 Establishing persistent connection for change notifications...")

	// Resolve authentication (same as in executePush)
	authResult, err := c.resolveAuth("push")
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	serverURL := authResult.ServerURL
	if c.config.ServerURL != "" && c.config.ServerURL != "wss://sqlrsync.com" {
		serverURL = c.config.ServerURL
	}

	remotePath := authResult.RemotePath
	if c.config.RemotePath != "" {
		remotePath = c.config.RemotePath
	}

	c.remoteClient, err = remote.New(&remote.Config{
		ServerURL:               serverURL + "/sapi/push/" + remotePath,
		PingPong:                true,
		Timeout:                 60000,
		AuthToken:               authResult.AccessToken,
		Logger:                  c.logger.Named("remote-notifications"),
		EnableTrafficInspection: c.config.Verbose,
	})
	if err != nil {
		return fmt.Errorf("failed to create remote client: %w", err)
	}
	defer c.remoteClient.Close()

	if err := c.remoteClient.Connect(); err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}

	// Wait briefly for CONFIG message to arrive with key type
	time.Sleep(500 * time.Millisecond)

	// Check key type - if it's a PULL key, we shouldn't be doing PUSH subscribe
	keyType := c.remoteClient.GetKeyType()
	if keyType == "PULL" {
		fmt.Println("⚠️  Warning: Using a PULL key - ignoring --waitIdle/--maxInterval/--minInterval settings")
		fmt.Println("   Switching to PULL subscription mode instead...")
		return c.executeSubscribe()
	}

	fmt.Println("ℹ️  Change notifications will be sent to server for analytics")

	// Create file watcher
	fileWatcher, err := watcher.NewWatcher(&watcher.Config{
		DatabasePath:          c.config.LocalPath,
		Logger:                c.logger.Named("watcher"),
		WriteNotificationFunc: c.sendWriteNotification,
	})
	if err != nil {
		return fmt.Errorf("failed to create file watcher: %w", err)
	}
	defer fileWatcher.Close()

	if err := fileWatcher.Start(); err != nil {
		return fmt.Errorf("failed to start file watcher: %w", err)
	}

	fmt.Printf("👀 Watching %s for changes...\n", c.config.LocalPath)
	fmt.Printf("   Wait idle: %v\n", waitIdleDuration)
	if maxIntervalDuration > 0 {
		fmt.Printf("   Max interval: %v\n", maxIntervalDuration)
	}
	if minIntervalDuration > 0 {
		fmt.Printf("   Min interval: %v\n", minIntervalDuration)
	}

	var waitTimer *time.Timer
	resetCount := 0

	for {
		select {
		case <-c.ctx.Done():
			fmt.Println("Subscription stopped by user.")
			return nil

		default:
		}

		// Check for maxInterval timeout even without file changes
		if maxIntervalDuration > 0 && time.Since(lastPushTime) >= maxIntervalDuration {
			fmt.Printf("⏰ Max interval (%v) reached - pushing...\n", maxIntervalDuration)

			if waitTimer != nil {
				waitTimer.Stop()
				waitTimer = nil
			}

			if err := c.executePushWithRetry(); err != nil {
				c.logger.Error("PUSH failed", zap.Error(err))
			} else {
				lastPushTime = time.Now()
				resetCount = 0
			}
			continue
		}

		// Wait for file change with timeout
		changeTime, err := fileWatcher.WaitForChange()
		if err != nil {
			if strings.Contains(err.Error(), "cancelled") {
				fmt.Println("Subscription stopped by user.")
				return nil
			}
			return fmt.Errorf("file watcher error: %w", err)
		}

		timeSinceLastPush := time.Since(lastPushTime)

		c.logger.Debug("File change detected",
			zap.Time("changeTime", changeTime),
			zap.Duration("timeSinceLastPush", timeSinceLastPush))

		// Check if we should push immediately due to maxInterval
		if maxIntervalDuration > 0 && timeSinceLastPush >= maxIntervalDuration {
			fmt.Printf("⏰ Max interval (%v) reached - pushing immediately...\n", maxIntervalDuration)

			if waitTimer != nil {
				waitTimer.Stop()
				waitTimer = nil
			}

			if err := c.executePushWithRetry(); err != nil {
				c.logger.Error("PUSH failed", zap.Error(err))
				// Continue watching despite error
			} else {
				lastPushTime = time.Now()
				resetCount = 0
			}
			continue
		}

		// Calculate timer duration: MAX(minInterval - timeSinceLastPush, waitIdle)
		timerDuration := waitIdleDuration
		if minIntervalDuration > 0 {
			remainingMinInterval := minIntervalDuration - timeSinceLastPush
			if remainingMinInterval > timerDuration {
				timerDuration = remainingMinInterval
			}
		}

		c.logger.Debug("Calculated timer duration",
			zap.Duration("timerDuration", timerDuration),
			zap.Duration("waitIdle", waitIdleDuration),
			zap.Duration("minInterval", minIntervalDuration),
			zap.Duration("timeSinceLastPush", timeSinceLastPush))

		// Reset or start the wait timer
		if waitTimer != nil {
			waitTimer.Stop()
			resetCount++
		} else {
			resetCount = 0
		}

		c.logger.Debug("Starting wait timer", zap.Duration("duration", timerDuration))
		waitTimer = time.AfterFunc(timerDuration, func() {
			fmt.Printf("💤 Timer expired after %v - pushing changes...\n", timerDuration)

			if err := c.executePushWithRetry(); err != nil {
				c.logger.Error("PUSH failed", zap.Error(err))
			} else {
				lastPushTime = time.Now()
				resetCount = 0
			}
		})
	}
}

// executePushWithRetry executes a push with exponential backoff on failure
func (c *Coordinator) executePushWithRetry() error {
	const maxRetries = 5
	delay := 5 * time.Second

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			c.logger.Info("Retrying PUSH", zap.Int("attempt", attempt+1), zap.Duration("delay", delay))
			time.Sleep(delay)
			delay *= 2 // Exponential backoff
		}

		if err := c.executePush(); err != nil {
			lastErr = err
			c.logger.Warn("PUSH attempt failed", zap.Error(err), zap.Int("attempt", attempt+1))

			// Report error to server if it's been more than 5 minutes of failures
			if attempt == maxRetries-1 {
				c.reportErrorToServer(err)
			}
			continue
		}

		// PUSH succeeded - reset session flag for next batch of changes
		c.sessionStarted = false
		if c.resendTimer != nil {
			c.resendTimer.Stop()
			c.resendTimer = nil
		}

		return nil
	}

	return fmt.Errorf("PUSH failed after %d attempts: %w", maxRetries, lastErr)
}

// reportErrorToServer sends error information to the server via HTTPS POST
func (c *Coordinator) reportErrorToServer(err error) {
	// TODO: Implement HTTPS POST to $server/sapi/$replicaID with error message
	c.logger.Error("Reporting error to server (not yet implemented)", zap.Error(err))
}

// sendWriteNotification sends a write detection notification to the server
// On first write: sends SQLRSYNC_CHANGED with waitIdle duration and sets sessionStarted=true
// On subsequent writes: if resend timer isn't running, starts timer for (waitIdle - 10s) to resend
func (c *Coordinator) sendWriteNotification(path string, timestamp time.Time) error {
	if c.remoteClient == nil {
		return nil // No remote client available yet
	}

	waitIdleDuration, err := ParseDuration(c.config.WaitIdle)
	if err != nil {
		return err
	}

	// If this is the first write of a session
	if !c.sessionStarted {
		// Send SQLRSYNC_CHANGED with waitIdle duration
		if err := c.remoteClient.SendChangedNotification(uint32(waitIdleDuration.Seconds())); err != nil {
			return fmt.Errorf("failed to send SQLRSYNC_CHANGED: %w", err)
		}

		c.sessionStarted = true
		c.lastChangedSent = timestamp

		c.logger.Debug("Sent initial SQLRSYNC_CHANGED",
			zap.Duration("waitIdle", waitIdleDuration))

		return nil
	}

	// Subsequent write - schedule resend 10 seconds before expiration if not already scheduled
	if c.resendTimer == nil {
		// Calculate when we need to resend: 10 seconds before the original would expire
		timeSinceLastSent := time.Since(c.lastChangedSent)
		timeUntilExpiration := waitIdleDuration - timeSinceLastSent
		resendDelay := timeUntilExpiration - (10 * time.Second)

		if resendDelay < 0 {
			resendDelay = 0 // Send immediately if we're past the 10s mark
		}

		c.resendTimer = time.AfterFunc(resendDelay, func() {
			c.onResendTimerFired(waitIdleDuration)
		})

		c.logger.Debug("Scheduled SQLRSYNC_CHANGED resend",
			zap.Duration("delay", resendDelay))
	}

	return nil
}

// onResendTimerFired is called when we need to resend SQLRSYNC_CHANGED 10s before expiration
func (c *Coordinator) onResendTimerFired(waitIdleDuration time.Duration) {
	// Calculate remaining time until push
	timeSinceLastSent := time.Since(c.lastChangedSent)
	remainingTime := waitIdleDuration - timeSinceLastSent

	if remainingTime < 0 {
		remainingTime = 0
	}

	// Send SQLRSYNC_CHANGED with remaining duration
	if err := c.remoteClient.SendChangedNotification(uint32(remainingTime.Seconds())); err != nil {
		c.logger.Error("Failed to resend SQLRSYNC_CHANGED", zap.Error(err))
		c.resendTimer = nil
		return
	}

	c.lastChangedSent = time.Now()
	c.resendTimer = nil

	c.logger.Debug("Resent SQLRSYNC_CHANGED with updated duration",
		zap.Duration("remainingTime", remainingTime))
}

// executeAutoMerge handles automatic merging when server has newer version
func (c *Coordinator) executeAutoMerge(localClient *bridge.BridgeClient, remoteClient *remote.Client, latestVersion string) error {
	// Step 1: Create temp file for merge
	tempFile, err := os.CreateTemp("", "sqlrsync-merge-local-*.sqlite")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	tempFile.Close()
	defer os.Remove(tempPath)

	fmt.Printf("📋 Step 1/5: Copying local database to temp file %s...\n", tempPath)

	// Step 2: Use LOCAL mode to copy current database to temp
	if err := localClient.RunDirectSync(tempPath); err != nil {
		return fmt.Errorf("failed to copy local database to temp: %w", err)
	}

	fmt.Printf("📥 Step 2/5: Pulling latest version %s from server over temp file...\n", latestVersion)

	// Step 3: PULL latest version over the temp file
	authResult, err := c.resolveAuth("pull")
	if err != nil {
		return fmt.Errorf("authentication failed for merge pull: %w", err)
	}

	serverURL := authResult.ServerURL
	if c.config.ServerURL != "" && c.config.ServerURL != "wss://sqlrsync.com" {
		serverURL = c.config.ServerURL
	}

	remotePath := authResult.RemotePath
	if c.config.RemotePath != "" {
		remotePath = c.config.RemotePath
	}

	// Create new remote client for pulling latest version
	pullClient, err := remote.New(&remote.Config{
		ServerURL:               serverURL + "/sapi/pull/" + remotePath,
		AuthToken:               authResult.AccessToken,
		ReplicaID:               authResult.ReplicaID,
		Timeout:                 8000,
		PingPong:                false,
		Logger:                  c.logger.Named("merge-pull"),
		Version:                 latestVersion,
		SendConfigCmd:           false,
		SendKeyRequest:          false,
		EnableTrafficInspection: c.config.Verbose,
	})
	if err != nil {
		return fmt.Errorf("failed to create pull client for merge: %w", err)
	}
	defer pullClient.Close()

	if err := pullClient.Connect(); err != nil {
		return fmt.Errorf("failed to connect for merge pull: %w", err)
	}

	// Create bridge client for temp file
	tempBridge, err := bridge.New(&bridge.BridgeConfig{
		DatabasePath:             tempPath,
		Logger:                   c.logger.Named("merge-temp"),
		EnableSQLiteRsyncLogging: c.config.Verbose,
	})
	if err != nil {
		return fmt.Errorf("failed to create bridge for temp file: %w", err)
	}
	defer tempBridge.Close()

	// Pull latest version over temp file
	readFunc := func(buffer []byte) (int, error) {
		return pullClient.Read(buffer)
	}
	writeFunc := func(data []byte) error {
		return pullClient.Write(data)
	}

	if err := tempBridge.RunPullSync(readFunc, writeFunc); err != nil {
		return fmt.Errorf("failed to pull latest version for merge: %w", err)
	}

	fmt.Println("🔍 Step 3/5: Generating diff between temp (latest) and local (your changes)...")

	// Step 4: Generate diff between temp (has latest) and local (has our changes)
	// tempPath has server's latest version
	// c.config.LocalPath has our local changes
	// We want to find what changed from temp to local (our changes on top of latest)
	diffResult, err := bridge.RunSQLDiff(tempPath, c.config.LocalPath)
	if err != nil {
		return fmt.Errorf("failed to generate diff: %w", err)
	}

	if !diffResult.HasChanges {
		fmt.Println("✅ Step 4/5: No changes detected - databases are identical")
		return nil // Nothing to merge
	}

	c.logger.Debug("Diff generated",
		zap.Int("operations", len(diffResult.Operations)),
		zap.Int("conflicts", len(diffResult.Conflicts)))

	// Step 5: Check for conflicts
	if len(diffResult.Conflicts) > 0 {
		fmt.Printf("❌ Step 4/5: Detected %d primary key conflict(s)\n", len(diffResult.Conflicts))

		// Send conflict notification to server
		return c.sendMergeConflictNotification(serverURL, remotePath, latestVersion, []byte(diffResult.SQL))
	}

	fmt.Println("✅ Step 4/5: No conflicts detected")
	fmt.Printf("📝 Step 5/5: Applying %d change(s) to local database...\n", len(diffResult.Operations))

	// Step 6: Apply the diff to the temp file (which has latest)
	// Then copy temp to local
	if err := bridge.ApplyDiff(tempPath, diffResult); err != nil {
		// If apply fails, fall back to simple copy
		c.logger.Warn("Failed to apply diff, using direct copy", zap.Error(err))
		if err := tempBridge.RunDirectSync(c.config.LocalPath); err != nil {
			return fmt.Errorf("failed to apply merge: %w", err)
		}
	} else {
		// Copy merged temp file to local
		if err := tempBridge.RunDirectSync(c.config.LocalPath); err != nil {
			return fmt.Errorf("failed to copy merged result: %w", err)
		}
	}

	fmt.Println("✅ Merge completed successfully")
	return nil
}

// sendMergeConflictNotification sends a merge conflict notification to the server
func (c *Coordinator) sendMergeConflictNotification(serverURL, replicaName, version string, diffData []byte) error {
	// Get hostname and wsID for notification
	hostname, _ := os.Hostname()
	wsID, _ := auth.GetWsID()

	// TODO: Implement HTTP POST to serverURL/sapi/notification/account/replicaName/
	// Message body: { type: "merge-conflict", diff: base64(diffData), versions: [...], hostname: hostname, wsID: wsID }

	fmt.Printf("❌ Merge conflict detected - server blocking until manual resolution\n")
	fmt.Printf("   Server: %s\n", serverURL)
	fmt.Printf("   Replica: %s\n", replicaName)
	fmt.Printf("   Version: %s\n", version)
	fmt.Printf("   Hostname: %s\n", hostname)
	fmt.Printf("   wsID: %s\n", wsID)

	return fmt.Errorf("merge conflict requires manual resolution")
}

// vacuumDatabase creates a vacuumed copy of the database in /tmp
// Returns the path to the temp file and a cleanup function
func (c *Coordinator) vacuumDatabase(dbPath string) (string, func(), error) {
	c.logger.Info("Creating vacuumed copy of database", zap.String("source", dbPath))

	// Create temp file in /tmp with pattern sqlrsync-vacuum-*
	tempFile, err := os.CreateTemp("/tmp", "sqlrsync-vacuum-*.sqlite")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tempPath := tempFile.Name()
	tempFile.Close()

	// Remove the temp file so VACUUM INTO can create it
	os.Remove(tempPath)

	cleanup := func() {
		if err := os.Remove(tempPath); err != nil {
			c.logger.Warn("Failed to remove temp vacuum file", zap.String("path", tempPath), zap.Error(err))
		}
	}

	// Open database using RunDirectSync to copy and vacuum
	// First, we'll use a direct file copy, then vacuum in place
	// Actually, we'll use bridge to copy the file directly, then vacuum the copy
	localClient, err := bridge.New(&bridge.BridgeConfig{
		DatabasePath:             dbPath,
		Logger:                   c.logger.Named("vacuum-copy"),
		EnableSQLiteRsyncLogging: c.config.Verbose,
	})
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to create bridge client: %w", err)
	}
	defer localClient.Close()

	// Copy database to temp location using RunDirectSync
	if err := localClient.RunDirectSync(tempPath); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("failed to copy database: %w", err)
	}

	c.logger.Info("Database copied, running VACUUM", zap.String("tempFile", tempPath))

	// Now run VACUUM on the temp file using the bridge's ExecuteVacuum function
	if err := bridge.ExecuteVacuum(tempPath); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("VACUUM failed: %w", err)
	}

	c.logger.Info("VACUUM complete", zap.String("tempFile", tempPath))

	// Get file sizes for reporting
	sourceInfo, _ := os.Stat(dbPath)
	tempInfo, _ := os.Stat(tempPath)
	if sourceInfo != nil && tempInfo != nil {
		reduction := float64(sourceInfo.Size()-tempInfo.Size()) / float64(sourceInfo.Size()) * 100
		fmt.Printf("📦 VACUUM complete: %s → %s (%.1f%% reduction)\n",
			formatBytes(sourceInfo.Size()),
			formatBytes(tempInfo.Size()),
			reduction)
	}

	return tempPath, cleanup, nil
}

// formatBytes formats bytes as human-readable string
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// Close cleanly shuts down the coordinator
func (c *Coordinator) Close() error {
	c.cancel()
	if c.subManager != nil {
		return c.subManager.Close()
	}
	return nil
}
