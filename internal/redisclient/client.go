// Package redisclient provides a Redis client wrapper with connection pooling
// and utility functions for the Smart Router application.
package redisclient

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
	"orchestration-api-go/internal/config"
)

// Client wraps redis.Client with application-specific configuration
type Client struct {
	client *redis.Client
}

// NewClient creates a new Redis client with connection pool configuration
func NewClient(config *config.Config) (*Client, error) {
	opt, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse Redis URL: %w", err)
	}

	// Configure connection pool settings
	opt.PoolSize = config.RedisPoolSize
	opt.MinIdleConns = config.RedisMinIdleConn
	opt.MaxRetries = config.RedisMaxRetries
	opt.DialTimeout = config.RedisDialTimeout

	client := redis.NewClient(opt)

	return &Client{
		client: client,
	}, nil
}

// Ping performs a health check on the Redis connection
func (c *Client) Ping(ctx context.Context) error {
	return c.client.Ping(ctx).Err()
}

// Close closes the Redis connection
func (c *Client) Close() error {
	return c.client.Close()
}

// GetRedis returns the underlying redis.Client for direct access
func (c *Client) GetRedis() *redis.Client {
	return c.client
}
