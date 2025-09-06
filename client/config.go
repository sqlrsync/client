package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// .config/sqlrsync/defaults.toml
type DefaultsConfig struct {
	Defaults struct {
		Server string `toml:"server"`
	} `toml:"defaults"`
}

// .config/sqlrsync/local-secrets.toml
type LocalSecretsConfig struct {
	SQLRsyncDatabases []SQLRsyncDatabase `toml:"sqlrsync-databases"`
}

type SQLRsyncDatabase struct {
	LocalPath                     string    `toml:"path,omitempty"`
	Server                        string    `toml:"server"`
	CustomerSuppliedEncryptionKey string    `toml:"customerSuppliedEncryptionKey,omitempty"`
	ReplicaID                     string    `toml:"replicaID"`
	RemotePath                    string    `toml:"remotePath,omitempty"`
	PushKey                       string    `toml:"pushKey,omitempty"`
	LastPush                      time.Time `toml:"lastPush,omitempty"`
}

// DashSQLRsync manages the -sqlrsync file for a database
type DashSQLRsync struct {
	DatabasePath string
	RemotePath   string
	PullKey      string
	Server       string
	ReplicaID    string
}

func GetConfigDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user home directory: %w", err)
	}
	return filepath.Join(homeDir, ".config", "sqlrsync"), nil
}

func GetDefaultsPath() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "defaults.toml"), nil
}

func GetLocalSecretsPath() (string, error) {
	configDir, err := GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "local-secrets.toml"), nil
}

func LoadDefaultsConfig() (*DefaultsConfig, error) {
	path, err := GetDefaultsPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Return default config if file doesn't exist
			config := &DefaultsConfig{}
			config.Defaults.Server = "wss://sqlrsync.com"
			return config, nil
		}
		return nil, fmt.Errorf("failed to read defaults config file %s: %w", path, err)
	}

	var config DefaultsConfig
	if _, err := toml.Decode(string(data), &config); err != nil {
		return nil, fmt.Errorf("failed to parse TOML defaults config: %w", err)
	}

	// Set default server if not specified
	if config.Defaults.Server == "" {
		config.Defaults.Server = "wss://sqlrsync.com"
	}

	return &config, nil
}

func SaveDefaultsConfig(config *DefaultsConfig) error {
	path, err := GetDefaultsPath()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create defaults config file %s: %w", path, err)
	}
	defer file.Close()

	encoder := toml.NewEncoder(file)
	if err := encoder.Encode(config); err != nil {
		return fmt.Errorf("failed to write defaults config: %w", err)
	}

	return nil
}

func LoadLocalSecretsConfig() (*LocalSecretsConfig, error) {
	path, err := GetLocalSecretsPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Return empty config if file doesn't exist
			return &LocalSecretsConfig{
				SQLRsyncDatabases: []SQLRsyncDatabase{},
			}, nil
		}
		return nil, fmt.Errorf("failed to read local-secrets config file %s: %w", path, err)
	}

	var config LocalSecretsConfig
	if _, err := toml.Decode(string(data), &config); err != nil {
		return nil, fmt.Errorf("failed to parse TOML local-secrets config: %w", err)
	}

	return &config, nil
}

func SaveLocalSecretsConfig(config *LocalSecretsConfig) error {
	path, err := GetLocalSecretsPath()
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create local-secrets config file %s: %w", path, err)
	}
	defer file.Close()

	// Set file permissions to 0600 (read/write for owner only)
	if err := file.Chmod(0600); err != nil {
		return fmt.Errorf("failed to set permissions on local-secrets config file: %w", err)
	}

	encoder := toml.NewEncoder(file)
	if err := encoder.Encode(config); err != nil {
		return fmt.Errorf("failed to write local-secrets config: %w", err)
	}

	return nil
}

func (c *LocalSecretsConfig) FindDatabaseByPath(path string) *SQLRsyncDatabase {
	for i := range c.SQLRsyncDatabases {
		if c.SQLRsyncDatabases[i].LocalPath == path {
			return &c.SQLRsyncDatabases[i]
		}
	}
	return nil
}

func (c *LocalSecretsConfig) UpdateOrAddDatabase(db SQLRsyncDatabase) {
	for i := range c.SQLRsyncDatabases {
		if c.SQLRsyncDatabases[i].LocalPath == db.LocalPath {
			// Update existing database
			c.SQLRsyncDatabases[i] = db
			return
		}
	}
	// Add new database
	c.SQLRsyncDatabases = append(c.SQLRsyncDatabases, db)
}

func (c *LocalSecretsConfig) RemoveDatabase(path string) {
	for i, db := range c.SQLRsyncDatabases {
		if db.LocalPath == path {
			// Remove database from slice
			c.SQLRsyncDatabases = append(c.SQLRsyncDatabases[:i], c.SQLRsyncDatabases[i+1:]...)
			return
		}
	}
}

// NewDashSQLRsync creates a new DashSQLRsync instance for the given database path
func NewDashSQLRsync(databasePath string) *DashSQLRsync {
	return &DashSQLRsync{
		DatabasePath: databasePath,
	}
}

// FilePath returns the path to the -sqlrsync file
func (d *DashSQLRsync) FilePath() string {
	return d.DatabasePath + "-sqlrsync"
}

// Exists checks if the -sqlrsync file exists
func (d *DashSQLRsync) Exists() bool {
	_, err := os.Stat(d.FilePath())
	return err == nil
}

// Read reads the -sqlrsync file and populates the struct fields
func (d *DashSQLRsync) Read() error {
	if !d.Exists() {
		return fmt.Errorf("file does not exist: %s", d.FilePath())
	}

	file, err := os.Open(d.FilePath())
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}

		if strings.HasPrefix(line, "sqlrsync ") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				d.RemotePath = parts[1]
			}

			for _, part := range parts {
				if strings.HasPrefix(part, "--pullKey=") {
					d.PullKey = strings.TrimPrefix(part, "--pullKey=")
				}
				if strings.HasPrefix(part, "--replicaID=") {
					d.ReplicaID = strings.TrimPrefix(part, "--replicaID=")
				}
				if strings.HasPrefix(part, "--server=") {
					d.Server = strings.TrimPrefix(part, "--server=")
				}
			}
			break
		}
	}

	return scanner.Err()
}

// Write writes the -sqlrsync file with the given remote path and pull key
func (d *DashSQLRsync) Write(remotePath string, replicaID string, pullKey string, serverURL string) error {
	d.RemotePath = remotePath
	d.PullKey = pullKey

	content := fmt.Sprintf(`#!/bin/bash
# https://sqlrsync.com/docs/-sqlrsync
sqlrsync %s --replicaID=%s --pullKey=%s --server=%s
`, remotePath, replicaID, pullKey, serverURL)

	if err := os.WriteFile(d.FilePath(), []byte(content), 0755); err != nil {
		return fmt.Errorf("failed to write -sqlrsync file: %w", err)
	}

	return nil
}

// Remove removes the -sqlrsync file if it exists
func (d *DashSQLRsync) Remove() error {
	if !d.Exists() {
		return nil
	}
	return os.Remove(d.FilePath())
}
