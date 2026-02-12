package models

import (
	"time"
)

type AllocationResult struct {
	PodName     string    `json:"pod_name"`
	WSURL       string    `json:"ws_url"`
	SourcePool  string    `json:"source_pool"`
	AllocatedAt time.Time `json:"allocated_at"`
	WasExisting bool      `json:"was_existing"`
}

type AllocationRequest struct {
	CallSID    string `json:"call_sid"`
	MerchantID string `json:"merchant_id,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Flow       string `json:"flow,omitempty"`
	Template   string `json:"template,omitempty"`
}

type ReleaseRequest struct {
	CallSID string `json:"call_sid"`
}

type ReleaseResult struct {
	Success        bool   `json:"success"`
	PodName        string `json:"pod_name"`
	ReleasedToPool string `json:"released_to_pool"`
	WasDraining    bool   `json:"was_draining"`
}

type DrainRequest struct {
	PodName string `json:"pod_name"`
}

type DrainResult struct {
	Success       bool   `json:"success"`
	PodName       string `json:"pod_name"`
	HasActiveCall bool   `json:"has_active_call"`
	Message       string `json:"message"`
}

// MerchantConfig holds allocation configuration for a specific merchant,
// stored in the Redis hash "voice:merchant:config".
//
// Fields:
//   - Tier:     The merchant's primary tier (e.g. "gold", "dedicated"). Kept for
//     backward compatibility; when Fallback is set it takes precedence.
//   - Pool:     The merchant's dedicated pool name. When set, the allocator
//     auto-prepends "merchant:{Pool}" to the fallback chain.
//   - Fallback: Ordered list of tier names to try during allocation. If empty,
//     the system-wide DefaultChain from Config is used. Entries must be
//     tier names from ParsedTierConfig (e.g. "gold", "standard", "basic").
//     The dedicated pool should NOT be listed here — it is auto-prepended.
type MerchantConfig struct {
	Tier     string   `json:"tier"`
	Pool     string   `json:"pool,omitempty"`
	Fallback []string `json:"fallback,omitempty"`
}

type PodMetadata struct {
	Tier string `json:"tier"`
	Name string `json:"name"`
}

type CallInfo struct {
	PodName     string `json:"pod_name"`
	SourcePool  string `json:"source_pool"`
	MerchantID  string `json:"merchant_id"`
	AllocatedAt string `json:"allocated_at"`
}

type StatusResponse struct {
	Pools       map[string]PoolInfo `json:"pools"`
	ActiveCalls int                 `json:"active_calls"`
	IsLeader    bool                `json:"is_leader"`
	Status      string              `json:"status"` // Added field
}

type PoolInfo struct {
	Type      string `json:"type"`
	Assigned  int    `json:"assigned"`
	Available int    `json:"available"`
}

type HealthResponse struct {
	Status string `json:"status"`
}
