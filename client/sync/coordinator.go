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
)

// Operation represents a sync operation type
type Operation int

const (
	OperationPull Operation = iota
	OperationPush
	OperationSubscribe
	OperationLocalSync
)

// CoordinatorConfig holds sync coordinator configuration
type CoordinatorConfig struct {
	ServerURL         string
	ProvidedAuthKey   string // Explicitly provided auth key
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
	WsID              string // Websocket ID for client identification
	ClientVersion     string // version of the client software
}

// Coordinator manages sync operations and subscriptions
type Coordinator struct {
	config       *CoordinatorConfig
	logger       *zap.Logger
	authResolver *auth.Resolver
	subManager   *subscription.Manager
	ctx          context.Context
	cancel       context.CancelFunc
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
		if authResult.AccessKey != "" {
			fmt.Printf(" - Access Key:     %s\n", color.GreenString(authResult.AccessKey))
		} else {
			fmt.Printf(" - Access Key:     %s\n", color.YellowString("(none)"))
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

	// Try explicit auth key first
	if c.config.ProvidedAuthKey != "" {
		return &auth.ResolveResult{
			AccessKey:  c.config.ProvidedAuthKey,
			ReplicaID:  c.config.ProvidedReplicaID,
			ServerURL:  c.config.ServerURL,
			RemotePath: c.config.RemotePath,
			LocalPath:  c.config.LocalPath,
		}, nil
	}

	result, err := c.authResolver.Resolve(req)
	if err != nil {
		return nil, err
	}

	// If prompting is needed for push operations
	if result.ShouldPrompt || operation == "push" {
		key, err := c.authResolver.PromptForKey(c.config.ServerURL, c.config.RemotePath, "PUSH")
		if err != nil {
			return nil, err
		}
		result.AccessKey = key
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
	c.subManager = subscription.NewManager(&subscription.ManagerConfig{
		ServerURL:             authResult.ServerURL,
		ReplicaPath:           authResult.RemotePath,
		AccessKey:             authResult.AccessKey,
		ReplicaID:             authResult.ReplicaID,
		WsID:                  c.config.WsID,
		ClientVersion:         c.config.ClientVersion,
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
		AuthKey:                 authResult.AccessKey,
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
		WsID:                    c.config.WsID, // Add websocket ID
		ClientVersion:           c.config.ClientVersion,
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
		if (strings.Contains(err.Error(), "key is not authorized") || strings.Contains(err.Error(), "404 Path not found")) && authResult.AccessKey == "" {
			key, err := c.authResolver.PromptForKey(c.config.ServerURL, c.config.RemotePath, "PULL")
			if err != nil {
				return fmt.Errorf("coordinator failed to get key interactively: %w", err)
			}
			c.config.ProvidedAuthKey = key
			return c.executePull(isSubscription)
		} else {

			return fmt.Errorf("coordinator failed to connect to server: %w", err)
		}
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
		AuthKey:                 authResult.AccessKey,
		Logger:                  c.logger.Named("remote"),
		EnableTrafficInspection: c.config.Verbose,
		LocalHostname:           localHostname,
		LocalAbsolutePath:       absLocalPath,
		InspectionDepth:         5,
		SendKeyRequest:          c.authResolver.CheckNeedsDashFile(c.config.LocalPath, remotePath),
		SendConfigCmd:           true,
		SetVisibility:           c.config.SetVisibility,
		CommitMessage:           c.config.CommitMessage,
		WsID:                    c.config.WsID, // Add websocket ID
		ClientVersion:           c.config.ClientVersion,
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
		return fmt.Errorf("push synchronization failed: %w", err)
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

// Close cleanly shuts down the coordinator
func (c *Coordinator) Close() error {
	c.cancel()
	if c.subManager != nil {
		return c.subManager.Close()
	}
	return nil
}
