package domain

import "time"

// Pod represents a Kubernetes pod in the system
type Pod struct {
	Name       string    `json:"name"`
	Namespace  string    `json:"namespace"`
	MerchantID string    `json:"merchant_id"`
	Status     PodStatus `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

// PodStatus represents the status of a pod
type PodStatus string

const (
	PodStatusPending   PodStatus = "pending"
	PodStatusRunning   PodStatus = "running"
	PodStatusSucceeded PodStatus = "succeeded"
	PodStatusFailed    PodStatus = "failed"
	PodStatusUnknown   PodStatus = "unknown"
)

// PodAllocationRequest represents a request to allocate a pod
type PodAllocationRequest struct {
	MerchantID string `json:"merchant_id" binding:"required"`
	PodCount   int    `json:"pod_count" binding:"required,min=1"`
}

// PodAllocationResponse represents a response to pod allocation
type PodAllocationResponse struct {
	MerchantID      string `json:"merchant_id"`
	RequestedCount  int    `json:"requested_count"`
	AllocatedCount  int    `json:"allocated_count"`
	AvailablePods   []Pod  `json:"available_pods"`
	Message         string `json:"message"`
}
