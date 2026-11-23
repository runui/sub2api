package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetEmailOTPSettings(c *gin.Context) {
	enabled, err := h.settingService.EmailOTPEnabled(c.Request.Context())
	if !response.ErrorFrom(c, err) {
		response.Success(c, gin.H{"enabled": enabled})
	}
}

func (h *SettingHandler) UpdateEmailOTPSettings(c *gin.Context) {
	var req struct {
		Enabled *bool `json:"enabled" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "enabled must be a boolean")
		return
	}
	if !response.ErrorFrom(c, h.settingService.SetEmailOTPEnabled(c.Request.Context(), *req.Enabled, h.emailService)) {
		response.Success(c, gin.H{"enabled": *req.Enabled})
	}
}
