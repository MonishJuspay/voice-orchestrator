package domain

import (
	"errors"
	"time"
)

// Merchant represents a merchant/tenant in the system
type Merchant struct {
	ID              string    `json:"id" db:"id"`
	Name            string    `json:"name" db:"name"`
	DesiredPodCount int       `json:"desired_pod_count" db:"desired_pod_count"`
	CreatedAt       time.Time `json:"created_at" db:"created_at"`
	UpdatedAt       time.Time `json:"updated_at" db:"updated_at"`
}

// Validate validates merchant data
func (m *Merchant) Validate() error {
	if m.Name == "" {
		return errors.New("merchant name is required")
	}
	if m.DesiredPodCount < 0 {
		return errors.New("desired pod count cannot be negative")
	}
	return nil
}

// MerchantCreateRequest represents a request to create a merchant
type MerchantCreateRequest struct {
	Name            string `json:"name" binding:"required"`
	DesiredPodCount int    `json:"desired_pod_count" binding:"min=0"`
}

// MerchantUpdateRequest represents a request to update a merchant
type MerchantUpdateRequest struct {
	Name            *string `json:"name,omitempty"`
	DesiredPodCount *int    `json:"desired_pod_count,omitempty" binding:"omitempty,min=0"`
}
