package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMerchantValidation(t *testing.T) {
	tests := []struct {
		name        string
		merchant    Merchant
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid merchant",
			merchant: Merchant{
				MerchantID:      "merchant-123",
				DesiredPodCount: 10,
			},
			expectError: false,
		},
		{
			name: "empty merchant_id",
			merchant: Merchant{
				MerchantID:      "",
				DesiredPodCount: 10,
			},
			expectError: true,
			errorMsg:    "merchant_id",
		},
		{
			name: "negative pod count",
			merchant: Merchant{
				MerchantID:      "merchant-123",
				DesiredPodCount: -1,
			},
			expectError: true,
			errorMsg:    "desired_pod_count",
		},
		{
			name: "zero pod count is valid",
			merchant: Merchant{
				MerchantID:      "merchant-123",
				DesiredPodCount: 0,
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.merchant.Validate()
			if tt.expectError {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errorMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCreateMerchantRequestValidation(t *testing.T) {
	tests := []struct {
		name        string
		req         CreateMerchantRequest
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid request",
			req: CreateMerchantRequest{
				MerchantID:      "merchant-123",
				DesiredPodCount: 10,
			},
			expectError: false,
		},
		{
			name: "empty merchant_id",
			req: CreateMerchantRequest{
				MerchantID:      "",
				DesiredPodCount: 10,
			},
			expectError: true,
			errorMsg:    "merchant_id",
		},
		{
			name: "negative pod count",
			req: CreateMerchantRequest{
				MerchantID:      "merchant-123",
				DesiredPodCount: -1,
			},
			expectError: true,
			errorMsg:    "desired_pod_count",
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

func TestUpdateMerchantRequestValidation(t *testing.T) {
	tests := []struct {
		name        string
		req         UpdateMerchantRequest
		expectError bool
		errorMsg    string
	}{
		{
			name: "valid request",
			req: UpdateMerchantRequest{
				DesiredPodCount: 20,
			},
			expectError: false,
		},
		{
			name: "zero pod count is valid",
			req: UpdateMerchantRequest{
				DesiredPodCount: 0,
			},
			expectError: false,
		},
		{
			name: "negative pod count",
			req: UpdateMerchantRequest{
				DesiredPodCount: -1,
			},
			expectError: true,
			errorMsg:    "desired_pod_count",
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

// TODO: Add tests for:
// - Merchant serialization/deserialization
// - Merchant business logic
// - Edge cases (very large pod counts, special characters in merchant_id)
