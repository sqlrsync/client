package auth

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
)

// ResolveResult contains the resolved authentication information
type ResolveResult struct {
	AuthToken    string
	ReplicaID    string
	ServerURL    string
	RemotePath   string
	LocalPath    string
	ShouldPrompt bool
}

// ResolveRequest contains the parameters for authentication resolution
type ResolveRequest struct {
	LocalPath        string
	RemotePath       string
	ServerURL        string
	ProvidedPullKey  string
	ProvidedPushKey  string
	ProvidedReplicaID string
	Operation        string // "pull", "push", "subscribe"
	Logger           *zap.Logger
}

// Resolver handles authentication and configuration resolution
type Resolver struct {
	logger *zap.Logger
}

// NewResolver creates a new authentication resolver
func NewResolver(logger *zap.Logger) *Resolver {
	return &Resolver{
		logger: logger,
	}
}

// Resolve determines the authentication method and configuration for an operation
func (r *Resolver) Resolve(req *ResolveRequest) (*ResolveResult, error) {
	result := &ResolveResult{
		ServerURL:  req.ServerURL,
		LocalPath:  req.LocalPath,
		RemotePath: req.RemotePath,
	}

	// 1. Try environment variable first
	if token := os.Getenv("SQLRSYNC_AUTH_TOKEN"); token != "" {
		r.logger.Debug("Using SQLRSYNC_AUTH_TOKEN from environment")
		result.AuthToken = token
		result.ReplicaID = req.ProvidedReplicaID
		return result, nil
	}

	// 2. Try explicitly provided keys
	if req.ProvidedPullKey != "" {
		r.logger.Debug("Using provided pull key")
		result.AuthToken = req.ProvidedPullKey
		result.ReplicaID = req.ProvidedReplicaID
		return result, nil
	}

	if req.ProvidedPushKey != "" {
		r.logger.Debug("Using provided push key")
		result.AuthToken = req.ProvidedPushKey
		result.ReplicaID = req.ProvidedReplicaID
		return result, nil
	}

	// 3. For operations with local paths, check stored configurations
	if req.LocalPath != "" {
		// Get absolute path for lookups
		absLocalPath, err := filepath.Abs(req.LocalPath)
		if err != nil {
			return nil, fmt.Errorf("failed to get absolute path: %w", err)
		}

		// Check local secrets config for push operations
		if req.Operation == "push" {
			if authResult, err := r.resolveFromLocalSecrets(absLocalPath, req.ServerURL, result); err == nil {
				return authResult, nil
			}
		}

		// Check -sqlrsync file for pull/subscribe operations
		if req.Operation == "pull" || req.Operation == "subscribe" {
			if authResult, err := r.resolveFromDashFile(absLocalPath, result); err == nil {
				return authResult, nil
			}
		}
	}

	// 4. For push operations, check if we need to prompt for admin key
	if req.Operation == "push" {
		if os.Getenv("SQLRSYNC_ADMIN_KEY") != "" {
			r.logger.Debug("Using SQLRSYNC_ADMIN_KEY from environment")
			result.AuthToken = os.Getenv("SQLRSYNC_ADMIN_KEY")
			result.ShouldPrompt = false
			return result, nil
		}

		// Need to prompt for admin key
		result.ShouldPrompt = true
		return result, nil
	}

	// 5. No authentication found
	return nil, fmt.Errorf("no authentication credentials found")
}

// resolveFromLocalSecrets attempts to resolve auth from local-secrets.toml
func (r *Resolver) resolveFromLocalSecrets(absLocalPath, serverURL string, result *ResolveResult) (*ResolveResult, error) {
	localSecretsConfig, err := LoadLocalSecretsConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load local secrets config: %w", err)
	}

	dbConfig := localSecretsConfig.FindDatabaseByPath(absLocalPath)
	if dbConfig == nil {
		return nil, fmt.Errorf("no database configuration found for path: %s", absLocalPath)
	}

	if dbConfig.PushKey == "" {
		return nil, fmt.Errorf("no push key found for database")
	}

	if dbConfig.Server != serverURL {
		return nil, fmt.Errorf("server URL mismatch: configured=%s, requested=%s", dbConfig.Server, serverURL)
	}

	r.logger.Debug("Found authentication in local secrets config")
	result.AuthToken = dbConfig.PushKey
	result.ReplicaID = dbConfig.ReplicaID
	result.RemotePath = dbConfig.RemotePath
	result.ServerURL = dbConfig.Server

	return result, nil
}

// resolveFromDashFile attempts to resolve auth from -sqlrsync file
func (r *Resolver) resolveFromDashFile(localPath string, result *ResolveResult) (*ResolveResult, error) {
	dashSQLRsync := NewDashSQLRsync(localPath)
	if !dashSQLRsync.Exists() {
		return nil, fmt.Errorf("no -sqlrsync file found for: %s", localPath)
	}

	if err := dashSQLRsync.Read(); err != nil {
		return nil, fmt.Errorf("failed to read -sqlrsync file: %w", err)
	}

	if dashSQLRsync.PullKey == "" {
		return nil, fmt.Errorf("no pull key found in -sqlrsync file")
	}

	r.logger.Debug("Found authentication in -sqlrsync file")
	result.AuthToken = dashSQLRsync.PullKey
	result.ReplicaID = dashSQLRsync.ReplicaID
	result.RemotePath = dashSQLRsync.RemotePath
	result.ServerURL = dashSQLRsync.Server

	return result, nil
}

// PromptForAdminKey prompts the user for an admin key
func (r *Resolver) PromptForAdminKey(serverURL string) (string, error) {
	httpServer := strings.Replace(serverURL, "ws", "http", 1)
	fmt.Println("No Key provided. Creating a new Replica? Get a key at " + httpServer + "/namespaces")
	fmt.Print("   Enter an Account Admin Key to create a new Replica: ")

	reader := bufio.NewReader(os.Stdin)
	token, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read admin key: %w", err)
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("admin key cannot be empty")
	}

	return token, nil
}

// SavePushResult saves the result of a successful push operation
func (r *Resolver) SavePushResult(localPath, serverURL, remotePath, replicaID, pushKey string) error {
	absLocalPath, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	localSecretsConfig, err := LoadLocalSecretsConfig()
	if err != nil {
		return fmt.Errorf("failed to load local secrets config: %w", err)
	}

	dbConfig := SQLRsyncDatabase{
		LocalPath:  absLocalPath,
		Server:     serverURL,
		ReplicaID:  replicaID,
		RemotePath: remotePath,
		PushKey:    pushKey,
	}

	localSecretsConfig.UpdateOrAddDatabase(dbConfig)

	if err := SaveLocalSecretsConfig(localSecretsConfig); err != nil {
		return fmt.Errorf("failed to save local secrets config: %w", err)
	}

	r.logger.Info("Saved push authentication to local secrets config")
	return nil
}

// SavePullResult saves the result of a successful pull operation
func (r *Resolver) SavePullResult(localPath, serverURL, remotePath, replicaID, pullKey string) error {
	dashSQLRsync := NewDashSQLRsync(localPath)

	localNameTree := strings.Split(localPath, "/")
	localName := localNameTree[len(localNameTree)-1]

	if err := dashSQLRsync.Write(remotePath, localName, replicaID, pullKey, serverURL); err != nil {
		return fmt.Errorf("failed to create -sqlrsync file: %w", err)
	}

	r.logger.Info("Created -sqlrsync file", zap.String("path", dashSQLRsync.FilePath()))
	return nil
}

// CheckNeedsDashFile determines if a -sqlrsync file should be created
func (r *Resolver) CheckNeedsDashFile(localPath, remotePath string) bool {
	dashSQLRsync := NewDashSQLRsync(localPath)
	if !dashSQLRsync.Exists() {
		return true
	}

	// Read existing file to check if remote path matches
	if err := dashSQLRsync.Read(); err != nil {
		return true // If we can't read it, recreate it
	}

	return dashSQLRsync.RemotePath != remotePath
}