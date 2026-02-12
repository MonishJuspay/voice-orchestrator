package allocator

import (
	"context"
	"errors"

	"orchestration-api-go/internal/models"
)

// Allocation errors
var (
	ErrNoPodsAvailable = errors.New("no pods available in any pool")
	ErrDrainingPod     = errors.New("pod is draining and cannot be allocated")
	ErrInvalidCallSID  = errors.New("invalid call SID")
)

// Interface defines the interface for pod allocation
type Interface interface {
	// Allocate assigns a pod for the given call SID, merchant ID, provider, flow, and template.
	// Provider is "twilio", "plivo", or "exotel". Flow is "v1" or "v2".
	// Template is the WebSocket path segment (e.g., "order-confirmation", "template").
	// These are used to build the correct provider-specific WebSocket path.
	Allocate(ctx context.Context, callSID, merchantID, provider, flow, template string) (*models.AllocationResult, error)
}


