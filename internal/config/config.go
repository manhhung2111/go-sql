package config

import (
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

func (c ServerConfig) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

type StorageConfig struct {
	DataDir string `yaml:"data_dir"`
}

type Config struct {
	Server  ServerConfig  `yaml:"server"`
	Storage StorageConfig `yaml:"storage"`
}

// DefaultDataDir is used when the config does not set storage.data_dir. It is
// relative to the server's working directory.
const DefaultDataDir = "data"

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}

	if cfg.Storage.DataDir == "" {
		cfg.Storage.DataDir = DefaultDataDir
	}

	return &cfg, nil
}
