package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

type DefaultsConfig struct {
	Defaults struct {
		Server string `toml:"server"`
	} `toml:"defaults"`
}

type LocalSecretsConfig struct {
	Local struct {
		Hostname                       string `toml:"hostname"`
		DefaultClientSideEncryptionKey string `toml:"defaultClientSideEncryptionKey"`
	} `toml:"local"`
	SQLRsyncDatabases []SQLRsyncDatabase `toml:"sqlrsync-databases"`
}

type SQLRsyncDatabase struct {
	Path                    string    `toml:"path"`
	ReplicaID               string    `toml:"replicaID,omitempty"`
	PrivatePushKey          string    `toml:"private-push-key,omitempty"`
	ClientSideEncryptionKey string    `toml:"clientSideEncryptionKey,omitempty"`
	LastUpdated             time.Time `toml:"lastUpdated,omitempty"`
	Server                  string    `toml:"server,omitempty"`
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
		if c.SQLRsyncDatabases[i].Path == path {
			return &c.SQLRsyncDatabases[i]
		}
	}
	return nil
}

func (c *LocalSecretsConfig) UpdateOrAddDatabase(db SQLRsyncDatabase) {
	for i := range c.SQLRsyncDatabases {
		if c.SQLRsyncDatabases[i].Path == db.Path {
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
		if db.Path == path {
			// Remove database from slice
			c.SQLRsyncDatabases = append(c.SQLRsyncDatabases[:i], c.SQLRsyncDatabases[i+1:]...)
			return
		}
	}
}

func (c *LocalSecretsConfig) SetHostname(hostname string) {
	c.Local.Hostname = hostname
}

func (c *LocalSecretsConfig) SetDefaultEncryptionKey(key string) {
	c.Local.DefaultClientSideEncryptionKey = key
}