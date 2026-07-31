package admin

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// UserWeeklyQuotaSyncHandler exposes the management surface for synchronizing
// user OpenAI weekly quota windows with a selected Codex account.
type UserWeeklyQuotaSyncHandler struct {
	syncService *service.UserWeeklyQuotaSyncService
}

func NewUserWeeklyQuotaSyncHandler(syncService *service.UserWeeklyQuotaSyncService) *UserWeeklyQuotaSyncHandler {
	return &UserWeeklyQuotaSyncHandler{syncService: syncService}
}

func (h *UserWeeklyQuotaSyncHandler) GetStatus(c *gin.Context) {
	if h == nil || h.syncService == nil {
		response.Error(c, http.StatusServiceUnavailable, "user weekly quota sync is unavailable")
		return
	}
	status, err := h.syncService.GetStatus(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}

func (h *UserWeeklyQuotaSyncHandler) UpdateConfig(c *gin.Context) {
	if h == nil || h.syncService == nil {
		response.Error(c, http.StatusServiceUnavailable, "user weekly quota sync is unavailable")
		return
	}
	var req service.UserWeeklyQuotaSyncConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "invalid request body")
		return
	}
	status, err := h.syncService.UpdateConfig(c.Request.Context(), &req)
	if err != nil {
		if errors.Is(err, service.ErrUserWeeklyQuotaSyncUnavailable) {
			response.ErrorFrom(c, err)
			return
		}
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, status)
}

func (h *UserWeeklyQuotaSyncHandler) ListSourceAccounts(c *gin.Context) {
	if h == nil || h.syncService == nil {
		response.Error(c, http.StatusServiceUnavailable, "user weekly quota sync is unavailable")
		return
	}
	accounts, err := h.syncService.ListSourceAccounts(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, map[string]any{"accounts": accounts})
}

func (h *UserWeeklyQuotaSyncHandler) CheckNow(c *gin.Context) {
	if h == nil || h.syncService == nil {
		response.Error(c, http.StatusServiceUnavailable, "user weekly quota sync is unavailable")
		return
	}
	result, err := h.syncService.CheckNow(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

type resetAllWeeklyQuotaRequest struct {
	StartAt string `json:"start_at" binding:"required"`
}

func (h *UserWeeklyQuotaSyncHandler) ResetAllAt(c *gin.Context) {
	if h == nil || h.syncService == nil {
		response.Error(c, http.StatusServiceUnavailable, "user weekly quota sync is unavailable")
		return
	}
	var req resetAllWeeklyQuotaRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "start_at is required")
		return
	}
	start, err := time.Parse(time.RFC3339, strings.TrimSpace(req.StartAt))
	if err != nil {
		response.BadRequest(c, "start_at must be RFC3339")
		return
	}
	affected, err := h.syncService.ResetAllAt(c.Request.Context(), start)
	if err != nil {
		if errors.Is(err, service.ErrUserWeeklyQuotaSyncUnavailable) || errors.Is(err, service.ErrUserWeeklyQuotaSyncBusy) {
			response.ErrorFrom(c, err)
			return
		}
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, map[string]any{
		"affected_users": affected,
		"window_start":   start.UTC().Truncate(time.Second),
	})
}
