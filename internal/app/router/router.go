package router

import (
	"github.com/gin-gonic/gin"
)

// setupRoutes configures all HTTP routes
func setupRoutes(r *gin.Engine, h *Handler) {
	// Health check endpoints
	r.GET("/health", h.Health)
	r.GET("/ready", h.Ready)

	// API v1 routes
	v1 := r.Group("/api/v1")
	{
		// Pod allocation endpoint
		v1.POST("/allocate", h.AllocatePod)

		// Admin endpoints (future implementation)
		admin := v1.Group("/admin")
		{
			admin.POST("/merchants", h.CreateMerchant)
			admin.GET("/merchants/:id", h.GetMerchant)
			admin.PUT("/merchants/:id", h.UpdateMerchant)
			admin.DELETE("/merchants/:id", h.DeleteMerchant)
		}
	}
}
