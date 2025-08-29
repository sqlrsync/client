package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	DefaultPrefix string `yaml:"prefix"`
	PrivateToken  string `yaml:"privateToken"`
}

type DatabaseConfig struct {
	ServerID       string   `yaml:"serverID"`
	PullToken      string   `yaml:"pullToken,omitempty"`
	PushToken      string   `yaml:"pushToken,omitempty"`
	ServerStateID  int      `yaml:"serverStateID"`
	LocalStateHash string   `yaml:"localStateHash"`
	Aliases        []string `yaml:"aliases,omitempty"`
}

type SecretsConfig struct {
	Config Config                    `yaml:"config"`
	Dbs    map[string]DatabaseConfig `yaml:"dbs"`
}

func LoadSecretsConfig(path string) (*SecretsConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	var config SecretsConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse YAML config: %w", err)
	}

	return &config, nil
}

func SaveSecretsConfig(config *SecretsConfig, path string) error {
	data, err := yaml.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config to YAML: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file %s: %w", path, err)
	}

	return nil
}

func GetDefaultSecretsPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user home directory: %w", err)
	}
	return filepath.Join(homeDir, ".config", "sqlrsync", "secrets.yml"), nil
}

func LoadDefaultSecretsConfig() (*SecretsConfig, error) {
	path, err := GetDefaultSecretsPath()
	if err != nil {
		return nil, err
	}
	return LoadSecretsConfig(path)
}

func SaveDefaultSecretsConfig(config *SecretsConfig) error {
	path, err := GetDefaultSecretsPath()
	if err != nil {
		return err
	}
	return SaveSecretsConfig(config, path)
}

func (sc *SecretsConfig) GetDatabaseConfig(localPath string) (DatabaseConfig, bool) {
	config, exists := sc.Dbs[localPath]
	return config, exists
}

func (sc *SecretsConfig) SetDatabaseConfig(localPath string, config DatabaseConfig) {
	if sc.Dbs == nil {
		sc.Dbs = make(map[string]DatabaseConfig)
	}
	sc.Dbs[localPath] = config
}

func (sc *SecretsConfig) RemoveDatabaseConfig(localPath string) {
	if sc.Dbs != nil {
		delete(sc.Dbs, localPath)
	}
}

func (sc *SecretsConfig) GetPrivateToken() string {
	return sc.Config.PrivateToken
}

func (sc *SecretsConfig) SetPrivateToken(token string) {
	sc.Config.PrivateToken = token
}
