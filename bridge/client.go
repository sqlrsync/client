package bridge

import (
	"fmt"

	"go.uber.org/zap"
)

// Config holds the configuration for the local SQLite client
type Config struct {
	DatabasePath string
	DryRun       bool
	Logger       *zap.Logger
}

// DatabaseInfo holds metadata about the SQLite database
type DatabaseInfo struct {
	PageSize    int
	PageCount   int
	JournalMode string
}

// ReadFunc defines the function signature for reading data from remote
type ReadFunc func(buffer []byte) (int, error)

// WriteFunc defines the function signature for writing data to remote
type WriteFunc func(data []byte) error

// Client handles local SQLite operations and CGO interactions
type Client struct {
	Config    *Config
	Logger    *zap.Logger
	ReadFunc  ReadFunc
	WriteFunc WriteFunc
}

// New creates a new local SQLite client
func New(config *Config) (*Client, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	if config.Logger == nil {
		return nil, fmt.Errorf("logger cannot be nil")
	}

	client := &Client{
		Config: config,
		Logger: config.Logger,
	}

	return client, nil
}

// GetDatabaseInfo retrieves metadata about the SQLite database
func (c *Client) GetDatabaseInfo() (*DatabaseInfo, error) {
	c.Logger.Debug("Getting database info", zap.String("path", c.Config.DatabasePath))

	info, err := cgoGetDatabaseInfo(c.Config.DatabasePath)
	if err != nil {
		return nil, err
	}

	c.Logger.Debug("Database info retrieved",
		zap.Int("pageSize", info.PageSize),
		zap.Int("pageCount", info.PageCount),
		zap.String("journalMode", info.JournalMode))

	return info, nil
}

// RunPushSync runs the origin-side synchronization with provided I/O functions
func (c *Client) RunPushSync(readFunc ReadFunc, writeFunc WriteFunc) error {
	c.Logger.Info("Starting origin sync", zap.String("database", c.Config.DatabasePath))

	// Store I/O functions for callbacks
	c.ReadFunc = readFunc
	c.WriteFunc = writeFunc

	if c.Config.DryRun {
		c.Logger.Info("Running in dry-run mode")
	}

	c.Logger.Debug("Calling C sqlite_rsync_run_origin")

	// Run the origin synchronization via CGO bridge
	err := cgoRunOriginSync(c.Config.DatabasePath, c.Config.DryRun, c)
	if err != nil {
		return err
	}

	c.Logger.Info("Origin sync completed successfully")
	return nil
}

// RunPullSync runs the replica-side synchronization with provided I/O functions
func (c *Client) RunPullSync(readFunc ReadFunc, writeFunc WriteFunc) error {
	c.Logger.Info("Starting replica sync", zap.String("database", c.Config.DatabasePath))

	// Store I/O functions for callbacks
	c.ReadFunc = readFunc
	c.WriteFunc = writeFunc

	if c.Config.DryRun {
		c.Logger.Info("Running in dry-run mode")
	}

	c.Logger.Debug("Calling C sqlite_rsync_run_replica")

	// Run the replica synchronization via CGO bridge
	// For replica sync, we pass empty origin path and use the database path as replica
	err := cgoRunReplicaSync("", c.Config.DatabasePath, c)
	if err != nil {
		return err
	}

	c.Logger.Info("Replica sync completed successfully")
	return nil
}

// RunDirectSync runs direct local synchronization between two SQLite files
func (c *Client) RunDirectSync(replicaPath string) error {
	c.Logger.Info("Starting direct local sync", 
		zap.String("origin", c.Config.DatabasePath),
		zap.String("replica", replicaPath))

	if c.Config.DryRun {
		c.Logger.Info("Running in dry-run mode")
	}

	verboseLevel := 0
	if c.Logger.Core().Enabled(zap.DebugLevel) {
		verboseLevel = 2
	} else if c.Logger.Core().Enabled(zap.InfoLevel) {
		verboseLevel = 1
	}

	// Run direct sync via CGO bridge
	err := cgoRunDirectSync(c.Config.DatabasePath, replicaPath, c.Config.DryRun, verboseLevel)
	if err != nil {
		return err
	}

	c.Logger.Info("Direct sync completed successfully")
	return nil
}

// Close cleans up resources
func (c *Client) Close() {
	c.Logger.Debug("Cleaning up local client resources")
	cgoCleanup()
}
