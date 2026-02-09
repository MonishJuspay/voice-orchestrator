package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPodValidation(t *testing.T) {
	tests := []struct {
		name        string
		pod         Pod
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid pod",
			pod: Pod{
				PodID:      "pod-123",
				MerchantID: "merchant-456",
				IP:         "10.0.1.5",
				Status:     "ready",
			},
			expectError: false,
		},
		{
			name: "empty pod_id",
			pod: Pod{
				PodID:      "",
				MerchantID: "merchant-456",
				IP:         "10.0.1.5",
				Status:     "ready",
			},
			expectError: true,
			errorMsg:    "pod_id",
		},
		{
			name: "empty merchant_id",
			pod: Pod{
				PodID:      "pod-123",
				MerchantID: "",
				IP:         "10.0.1.5",
				Status:     "ready",
			},
			expectError: true,
			errorMsg:    "merchant_id",
		},
		{
			name: "empty IP",
			pod: Pod{
				PodID:      "pod-123",
				MerchantID: "merchant-456",
				IP:         "",
				Status:     "ready",
			},
			expectError: true,
			errorMsg:    "ip",
		},
		{
			name: "empty status",
			pod: Pod{
				PodID:      "pod-123",
				MerchantID: "merchant-456",
				IP:         "10.0.1.5",
				Status:     "",
			},
			expectError: true,
			errorMsg:    "status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.pod.Validate()
			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestAllocationRequestValidation(t *testing.T) {
	tests := []struct {
		name        string
		req         AllocationRequest
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid request",
			req: AllocationRequest{
				MerchantID: "merchant-123",
				PodCount:   5,
			},
			expectError: false,
		},
		{
			name: "empty merchant_id",
			req: AllocationRequest{
				MerchantID: "",
				PodCount:   5,
			},
			expectError: true,
			errorMsg:    "merchant_id",
		},
		{
			name: "zero pod count",
			req: AllocationRequest{
				MerchantID: "merchant-123",
				PodCount:   0,
			},
			expectError: true,
			errorMsg:    "pod_count",
		},
		{
			name: "negative pod count",
			req: AllocationRequest{
				MerchantID: "merchant-123",
				PodCount:   -1,
			},
			expectError: true,
			errorMsg:    "pod_count",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestAllocationResponse(t *testing.T) {
	t.Run("valid allocation response", func(t *testing.T) {
		pods := []Pod{
			{
				PodID:      "pod-1",
				MerchantID: "merchant-123",
				IP:         "10.0.1.1",
				Status:     "ready",
			},
			{
				PodID:      "pod-2",
				MerchantID: "merchant-123",
				IP:         "10.0.1.2",
				Status:     "ready",
			},
		}

		resp := AllocationResponse{
			MerchantID:     "merchant-123",
			AllocatedPods:  pods,
			AllocatedCount: len(pods),
		}

		assert.Equal(t, "merchant-123", resp.MerchantID)
		assert.Equal(t, 2, resp.AllocatedCount)
		assert.Len(t, resp.AllocatedPods, 2)
	})
}

// TODO: Add tests for:
// - Pod lifecycle states
// - Pod IP validation (valid IP format)
// - Allocation response edge cases
// - Pod status transitions
