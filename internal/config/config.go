package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config holds all application configuration
type Config struct {
	// Server configuration (Router only)
	ServerPort string
	ServerHost string

	// Database configuration
	PostgresURL string
	RedisURL    string

	// Kubernetes configuration
	K8sNamespace      string
	K8sInCluster      bool
	K8sKubeConfigPath string

	// Pool Manager configuration
	ReconcileIntervalSeconds int

	// Logging configuration
	LogLevel  string
	LogFormat string

	// Application metadata
	AppName    string
	AppVersion string
}

// Load loads configuration from environment variables
func Load() (*Config, error) {
	cfg := &Config{
		ServerPort:               getEnv("SERVER_PORT", "8080"),
		ServerHost:               getEnv("SERVER_HOST", "0.0.0.0"),
		PostgresURL:              getEnv("POSTGRES_URL", ""),
		RedisURL:                 getEnv("REDIS_URL", "redis://localhost:6379/0"),
		K8sNamespace:             getEnv("K8S_NAMESPACE", "default"),
		K8sInCluster:             getEnvBool("K8S_IN_CLUSTER", false),
		K8sKubeConfigPath:        getEnv("K8S_KUBECONFIG_PATH", ""),
		ReconcileIntervalSeconds: getEnvInt("RECONCILE_INTERVAL_SECONDS", 10),
		LogLevel:                 getEnv("LOG_LEVEL", "info"),
		LogFormat:                getEnv("LOG_FORMAT", "json"),
		AppName:                  "voice-orchestrator",
		AppVersion:               getEnv("APP_VERSION", "dev"),
	}

	// Validate required configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}

// Validate checks if required configuration is present
func (c *Config) Validate() error {
	// PostgresURL is required for production
	if c.PostgresURL == "" && os.Getenv("ENV") == "production" {
		return fmt.Errorf("POSTGRES_URL is required in production")
	}

	// Validate log level
	validLogLevels := map[string]bool{
		"debug": true,
		"info":  true,
		"warn":  true,
		"error": true,
	}
	if !validLogLevels[c.LogLevel] {
		return fmt.Errorf("invalid log level: %s (must be debug/info/warn/error)", c.LogLevel)
	}

	return nil
}

// GetServerAddress returns the full server address
func (c *Config) GetServerAddress() string {
	return c.ServerHost + ":" + c.ServerPort
}

// getEnv retrieves an environment variable or returns a default value
func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// getEnvBool retrieves a boolean environment variable or returns a default value
func getEnvBool(key string, defaultVal bool) bool {
	if val := os.Getenv(key); val != "" {
		b, err := strconv.ParseBool(val)
		if err != nil {
			return defaultVal
		}
		return b
	}
	return defaultVal
}

// getEnvInt retrieves an integer environment variable or returns a default value
func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		i, err := strconv.Atoi(val)
		if err != nil {
			return defaultVal
		}
		return i
	}
	return defaultVal
}
