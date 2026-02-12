package releaser

import (
	"context"
	"errors"

	"orchestration-api-go/internal/models"
)

// Release errors
var (
	ErrCallNotFound = errors.New("call not found")
)

// Interface defines the interface for pod release
type Interface interface {
	// Release releases a pod back to its source pool using call SID
	// Returns the release result or an error if release fails
	Release(ctx context.Context, callSID string) (*models.ReleaseResult, error)
}


