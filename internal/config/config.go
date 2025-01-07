package config

import (
	"fmt"
	"gopkg.in/yaml.v2"
	"os"
)

type Config struct {
	Database struct {
		Host       string `yaml:"host"`
		Port       int    `yaml:"port"`
		User       string `yaml:"user"`
		Password   string `yaml:"password"`
		Name       string `yaml:"name"`
		TimeoutSec int    `yaml:"timeout_sec"`
	} `yaml:"database"`

	API struct {
		Port int `yaml:"port"`
	} `yaml:"api"`

	Monitoring struct {
		IntervalSec int `yaml:"interval_sec"`
	} `yaml:"monitoring"`

	Logging struct {
		Level  string `yaml:"level"`
		Format string `yaml:"format"`
		File   struct {
			Path       string `yaml:"path"`
			MaxSize    int    `yaml:"max_size"`
			MaxBackups int    `yaml:"max_backups"`
			MaxAge     int    `yaml:"max_age"`
			Compress   bool   `yaml:"compress"`
		} `yaml:"file"`
	} `yaml:"logging"`
}

func (c *Config) Validate() error {
	if c.Database.Host == "" {
		return fmt.Errorf("database host is required")
	}
	if c.Database.Port < 1 || c.Database.Port > 65535 {
		return fmt.Errorf("invalid database port: %d", c.Database.Port)
	}
	if c.Database.User == "" {
		return fmt.Errorf("database user is required")
	}
	if c.Database.Password == "" {
		return fmt.Errorf("database password is required")
	}
	if c.Database.Name == "" {
		return fmt.Errorf("database name is required")
	}
	if c.Database.TimeoutSec <= 0 {
		return fmt.Errorf("invalid database timeout: %d", c.Database.TimeoutSec)
	}

	if c.API.Port < 1 || c.API.Port > 65535 {
		return fmt.Errorf("invalid API port: %d", c.API.Port)
	}

	if c.Monitoring.IntervalSec <= 0 {
		return fmt.Errorf("invalid monitoring interval: %d", c.Monitoring.IntervalSec)
	}

	validLevels := map[string]bool{
		"debug":  true,
		"info":   true,
		"warn":   true,
		"error":  true,
		"dpanic": true,
		"panic":  true,
		"fatal":  true,
	}
	if !validLevels[c.Logging.Level] {
		return fmt.Errorf("invalid log level: %s", c.Logging.Level)
	}

	validFormats := map[string]bool{
		"json":    true,
		"console": true,
	}
	if !validFormats[c.Logging.Format] {
		return fmt.Errorf("invalid log format: %s", c.Logging.Format)
	}

	if c.Logging.File.MaxSize < 0 {
		return fmt.Errorf("invalid log max size: %d", c.Logging.File.MaxSize)
	}
	if c.Logging.File.MaxBackups < 0 {
		return fmt.Errorf("invalid log max backups: %d", c.Logging.File.MaxBackups)
	}
	if c.Logging.File.MaxAge < 0 {
		return fmt.Errorf("invalid log max age: %d", c.Logging.File.MaxAge)
	}

	return nil
}

func (c *Config) setDefaults() {
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}
	if c.Logging.File.Path == "" {
		c.Logging.File.Path = "logs/tunnel-manager.log"
	}
	if c.Logging.File.MaxSize <= 0 {
		c.Logging.File.MaxSize = 100
	}
	if c.Logging.File.MaxBackups <= 0 {
		c.Logging.File.MaxBackups = 5
	}
	if c.Logging.File.MaxAge <= 0 {
		c.Logging.File.MaxAge = 30
	}
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("error reading config file: %w", err)
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("error parsing config file: %w", err)
	}

	config.setDefaults()

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &config, nil
}
