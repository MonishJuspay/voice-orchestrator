package postgres

import (
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

// Client wraps the Postgres client
type Client struct {
	db *sqlx.DB
}

// NewClient creates a new Postgres client
func NewClient(url string) (*Client, error) {
	// TODO: Implement Postgres client initialization
	// 1. Parse connection string
	// 2. Open connection with sqlx
	// 3. Ping to verify connection
	// 4. Set connection pool settings

	return nil, fmt.Errorf("not implemented: create Postgres client with URL %s", url)
}

// Ping checks if Postgres is reachable
func (c *Client) Ping() error {
	// TODO: Implement ping
	return fmt.Errorf("not implemented: ping Postgres")
}

// Close closes the Postgres connection
func (c *Client) Close() error {
	// TODO: Implement close
	return fmt.Errorf("not implemented: close Postgres connection")
}

// GetDB returns the underlying database connection
func (c *Client) GetDB() *sqlx.DB {
	return c.db
}

// BeginTx starts a new transaction
func (c *Client) BeginTx() (*sql.Tx, error) {
	// TODO: Implement begin transaction
	return nil, fmt.Errorf("not implemented: begin transaction")
}
