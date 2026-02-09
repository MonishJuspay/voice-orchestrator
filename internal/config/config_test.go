package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig(t *testing.T) {
	// Save original env vars
	originalEnv := map[string]string{
		"ENVIRONMENT":             os.Getenv("ENVIRONMENT"),
		"LOG_LEVEL":               os.Getenv("LOG_LEVEL"),
		"HTTP_PORT":               os.Getenv("HTTP_PORT"),
		"REDIS_ADDR":              os.Getenv("REDIS_ADDR"),
		"POSTGRES_HOST":           os.Getenv("POSTGRES_HOST"),
		"RECONCILE_INTERVAL":      os.Getenv("RECONCILE_INTERVAL"),
	}

	// Restore env vars after test
	defer func() {
		for key, value := range originalEnv {
			if value == "" {
				os.Unsetenv(key)
			} else {
				os.Setenv(key, value)
			}
		}
	}()

	t.Run("load with defaults", func(t *testing.T) {
		// Clear all env vars
		os.Clearenv()

		cfg := Load()

		assert.Equal(t, "development", cfg.Environment)
		assert.Equal(t, "info", cfg.LogLevel)
		assert.Equal(t, "8080", cfg.HTTPPort)
		assert.Equal(t, 30*time.Second, cfg.HTTPReadTimeout)
		assert.Equal(t, "localhost:6379", cfg.RedisAddr)
		assert.Equal(t, "localhost", cfg.PostgresHost)
		assert.Equal(t, "5432", cfg.PostgresPort)
		assert.Equal(t, 10*time.Second, cfg.ReconcileInterval)
	})

	t.Run("load with custom env vars", func(t *testing.T) {
		os.Setenv("ENVIRONMENT", "production")
		os.Setenv("LOG_LEVEL", "debug")
		os.Setenv("HTTP_PORT", "9090")
		os.Setenv("HTTP_READ_TIMEOUT", "60s")
		os.Setenv("REDIS_ADDR", "redis.example.com:6379")
		os.Setenv("REDIS_PASSWORD", "secret")
		os.Setenv("POSTGRES_HOST", "postgres.example.com")
		os.Setenv("POSTGRES_PORT", "5433")
		os.Setenv("POSTGRES_DB", "custom_db")
		os.Setenv("RECONCILE_INTERVAL", "30s")

		cfg := Load()

		assert.Equal(t, "production", cfg.Environment)
		assert.Equal(t, "debug", cfg.LogLevel)
		assert.Equal(t, "9090", cfg.HTTPPort)
		assert.Equal(t, 60*time.Second, cfg.HTTPReadTimeout)
		assert.Equal(t, "redis.example.com:6379", cfg.RedisAddr)
		assert.Equal(t, "secret", cfg.RedisPassword)
		assert.Equal(t, "postgres.example.com", cfg.PostgresHost)
		assert.Equal(t, "5433", cfg.PostgresPort)
		assert.Equal(t, "custom_db", cfg.PostgresDB)
		assert.Equal(t, 30*time.Second, cfg.ReconcileInterval)
	})

	t.Run("invalid duration fallback to default", func(t *testing.T) {
		os.Clearenv()
		os.Setenv("HTTP_READ_TIMEOUT", "invalid")
		os.Setenv("RECONCILE_INTERVAL", "not-a-duration")

		cfg := Load()

		// Should fall back to defaults when parsing fails
		assert.Equal(t, 30*time.Second, cfg.HTTPReadTimeout)
		assert.Equal(t, 10*time.Second, cfg.ReconcileInterval)
	})
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name        string
		cfg         Config
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid config",
			cfg: Config{
				Environment:  "production",
				LogLevel:     "info",
				HTTPPort:     "8080",
				RedisAddr:    "localhost:6379",
				PostgresHost: "localhost",
				PostgresPort: "5432",
				PostgresDB:   "voice_orchestrator",
				PostgresUser: "postgres",
			},
			expectError: false,
		},
		{
			name: "missing postgres host",
			cfg: Config{
				Environment:  "production",
				LogLevel:     "info",
				HTTPPort:     "8080",
				RedisAddr:    "localhost:6379",
				PostgresHost: "",
				PostgresPort: "5432",
				PostgresDB:   "voice_orchestrator",
				PostgresUser: "postgres",
			},
			expectError: true,
			errorMsg:    "postgres_host",
		},
		{
			name: "missing redis addr",
			cfg: Config{
				Environment:  "production",
				LogLevel:     "info",
				HTTPPort:     "8080",
				RedisAddr:    "",
				PostgresHost: "localhost",
				PostgresPort: "5432",
				PostgresDB:   "voice_orchestrator",
				PostgresUser: "postgres",
			},
			expectError: true,
			errorMsg:    "redis_addr",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TODO: Add tests for:
// - Config hot reload (if implemented)
// - K8s config loading (in-cluster vs kubeconfig)
// - Environment-specific defaults
