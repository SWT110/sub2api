package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

const (
	userWeeklyQuotaSyncDefaultPollInterval = 300 * time.Second
	userWeeklyQuotaSyncMinPollInterval     = 60 * time.Second
	userWeeklyQuotaSyncMaxPollInterval     = time.Hour
	userWeeklyQuotaSyncTickInterval        = time.Minute
	userWeeklyQuotaSyncLeaderLockKey       = "user:weekly-quota-sync:leader"
	userWeeklyQuotaSyncLeaderLockTTL       = 2 * time.Minute
	userWeeklyQuotaSyncTargetWindow        = 7 * 24 * time.Hour
	userWeeklyQuotaSyncWindowTolerance     = 24 * time.Hour
)

var (
	ErrUserWeeklyQuotaSyncUnavailable = infraerrors.ServiceUnavailable(
		"USER_WEEKLY_QUOTA_SYNC_UNAVAILABLE", "user weekly quota sync is unavailable",
	)
	ErrUserWeeklyQuotaSyncSourceInvalid = infraerrors.BadRequest(
		"USER_WEEKLY_QUOTA_SYNC_SOURCE_INVALID", "source account must be an active non-shadow OpenAI OAuth account",
	)
	ErrUserWeeklyQuotaSyncNoWeeklyLimit = infraerrors.BadRequest(
		"USER_WEEKLY_QUOTA_SYNC_NO_WEEKLY_LIMIT", "the source account did not return a seven-day Codex rate-limit window",
	)
	ErrUserWeeklyQuotaSyncBusy = infraerrors.Conflict(
		"USER_WEEKLY_QUOTA_SYNC_BUSY", "another instance is checking the user weekly quota sync",
	)
)

// UserWeeklyQuotaSyncConfig controls the optional automatic synchronization.
// SourceAccountID refers to an account already connected through the admin
// account manager; no browser login or credential duplication is required.
type UserWeeklyQuotaSyncConfig struct {
	Enabled             bool  `json:"enabled"`
	SourceAccountID     int64 `json:"source_account_id"`
	PollIntervalSeconds int   `json:"poll_interval_seconds"`
}

// UserWeeklyQuotaSyncState is operational state persisted independently from
// config so routine polling never overwrites an administrator's settings.
type UserWeeklyQuotaSyncState struct {
	SourceAccountID       int64      `json:"source_account_id"`
	ObservedResetAt       *time.Time `json:"observed_reset_at,omitempty"`
	ObservedWindowSeconds int64      `json:"observed_window_seconds,omitempty"`
	// PendingResetAt is a newly observed upstream reset boundary that must be
	// returned once more before a mass reset is allowed. Persisting it makes the
	// confirmation survive service restarts and prevents one bad response from
	// clearing every user's quota.
	PendingResetAt    *time.Time `json:"pending_reset_at,omitempty"`
	LastCheckedAt     *time.Time `json:"last_checked_at,omitempty"`
	LastTriggeredAt   *time.Time `json:"last_triggered_at,omitempty"`
	LastWindowStart   *time.Time `json:"last_window_start,omitempty"`
	LastAffectedUsers int        `json:"last_affected_users"`
	LastError         string     `json:"last_error,omitempty"`
}

// UserWeeklyQuotaSyncStatus is returned to the admin UI.
type UserWeeklyQuotaSyncStatus struct {
	Config UserWeeklyQuotaSyncConfig `json:"config"`
	State  UserWeeklyQuotaSyncState  `json:"state"`
}

// UserWeeklyQuotaSyncAccount is a safe source-account projection for the UI.
type UserWeeklyQuotaSyncAccount struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Eligible bool   `json:"eligible"`
}

// UserWeeklyQuotaSyncCheckResult describes one manual or scheduled check.
type UserWeeklyQuotaSyncCheckResult struct {
	BaselineInitialized  bool                      `json:"baseline_initialized"`
	AwaitingConfirmation bool                      `json:"awaiting_confirmation"`
	ResetDetected        bool                      `json:"reset_detected"`
	ResetAt              time.Time                 `json:"reset_at"`
	WindowStart          time.Time                 `json:"window_start"`
	AffectedUsers        int                       `json:"affected_users"`
	Status               UserWeeklyQuotaSyncStatus `json:"status"`
}

// userWeeklyQuotaBulkResetter is intentionally an optional extension of the
// standard quota repository port. Most request-path fakes only need the base
// interface; production's repository adapter implements this capability.
type userWeeklyQuotaBulkResetter interface {
	ResetWeeklyWindowForPlatform(ctx context.Context, platform string, newStart time.Time) ([]int64, error)
}

// UserWeeklyQuotaSyncService monitors one OpenAI OAuth/Codex account's actual
// seven-day reset boundary and mirrors it into configured user OpenAI quotas.
type UserWeeklyQuotaSyncService struct {
	settingRepo  SettingRepository
	accountRepo  AccountRepository
	quotaService interface {
		QueryUsage(context.Context, int64) (*OpenAIQuotaUsage, error)
	}
	bulkResetter  userWeeklyQuotaBulkResetter
	cacheResetter UserPlatformQuotaWeeklyCacheResetter
	lockCache     LeaderLockCache
	db            *sql.DB
	instanceID    string
	now           func() time.Time

	parentCtx    context.Context
	parentCancel context.CancelFunc
	mu           sync.Mutex
	cycleMu      sync.Mutex
	started      bool
	stopped      bool
	wg           sync.WaitGroup
}

func NewUserWeeklyQuotaSyncService(
	settingRepo SettingRepository,
	accountRepo AccountRepository,
	quotaService *OpenAIQuotaService,
	quotaRepo UserPlatformQuotaRepository,
	cache BillingCache,
) *UserWeeklyQuotaSyncService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &UserWeeklyQuotaSyncService{
		settingRepo:  settingRepo,
		accountRepo:  accountRepo,
		quotaService: quotaService,
		instanceID:   uuid.NewString(),
		now:          time.Now,
		parentCtx:    ctx,
		parentCancel: cancel,
	}
	if resetter, ok := quotaRepo.(userWeeklyQuotaBulkResetter); ok {
		s.bulkResetter = resetter
	}
	if resetter, ok := cache.(UserPlatformQuotaWeeklyCacheResetter); ok {
		s.cacheResetter = resetter
	}
	return s
}

func (s *UserWeeklyQuotaSyncService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

func defaultUserWeeklyQuotaSyncConfig() UserWeeklyQuotaSyncConfig {
	return UserWeeklyQuotaSyncConfig{PollIntervalSeconds: int(userWeeklyQuotaSyncDefaultPollInterval.Seconds())}
}

func normalizeUserWeeklyQuotaSyncConfig(cfg *UserWeeklyQuotaSyncConfig) {
	if cfg == nil {
		return
	}
	if cfg.PollIntervalSeconds == 0 {
		cfg.PollIntervalSeconds = int(userWeeklyQuotaSyncDefaultPollInterval.Seconds())
	}
}

func validateUserWeeklyQuotaSyncConfig(cfg *UserWeeklyQuotaSyncConfig) error {
	if cfg == nil {
		return errors.New("weekly quota sync config is required")
	}
	if cfg.PollIntervalSeconds < int(userWeeklyQuotaSyncMinPollInterval.Seconds()) || cfg.PollIntervalSeconds > int(userWeeklyQuotaSyncMaxPollInterval.Seconds()) {
		return fmt.Errorf("poll_interval_seconds must be between %d and %d", int(userWeeklyQuotaSyncMinPollInterval.Seconds()), int(userWeeklyQuotaSyncMaxPollInterval.Seconds()))
	}
	if cfg.Enabled && cfg.SourceAccountID <= 0 {
		return errors.New("source_account_id is required when sync is enabled")
	}
	if cfg.SourceAccountID < 0 {
		return errors.New("source_account_id must be positive")
	}
	return nil
}

func (s *UserWeeklyQuotaSyncService) GetConfig(ctx context.Context) (*UserWeeklyQuotaSyncConfig, error) {
	defaults := defaultUserWeeklyQuotaSyncConfig()
	if s == nil || s.settingRepo == nil {
		return &defaults, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyUserWeeklyQuotaSyncConfig)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return &defaults, nil
		}
		return nil, fmt.Errorf("get weekly quota sync config: %w", err)
	}
	cfg := defaults
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return nil, fmt.Errorf("parse weekly quota sync config: %w", err)
		}
	}
	normalizeUserWeeklyQuotaSyncConfig(&cfg)
	return &cfg, nil
}

func (s *UserWeeklyQuotaSyncService) GetState(ctx context.Context) (*UserWeeklyQuotaSyncState, error) {
	state := UserWeeklyQuotaSyncState{}
	if s == nil || s.settingRepo == nil {
		return &state, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyUserWeeklyQuotaSyncState)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return &state, nil
		}
		return nil, fmt.Errorf("get weekly quota sync state: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return &state, nil
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return &UserWeeklyQuotaSyncState{}, nil
	}
	return &state, nil
}

func (s *UserWeeklyQuotaSyncService) saveState(ctx context.Context, state *UserWeeklyQuotaSyncState) error {
	if s == nil || s.settingRepo == nil {
		return ErrUserWeeklyQuotaSyncUnavailable
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.settingRepo.Set(ctx, SettingKeyUserWeeklyQuotaSyncState, string(raw))
}

func (s *UserWeeklyQuotaSyncService) GetStatus(ctx context.Context) (*UserWeeklyQuotaSyncStatus, error) {
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	state, err := s.GetState(ctx)
	if err != nil {
		return nil, err
	}
	return &UserWeeklyQuotaSyncStatus{Config: *cfg, State: *state}, nil
}

func (s *UserWeeklyQuotaSyncService) UpdateConfig(ctx context.Context, cfg *UserWeeklyQuotaSyncConfig) (*UserWeeklyQuotaSyncStatus, error) {
	if s == nil || s.settingRepo == nil {
		return nil, ErrUserWeeklyQuotaSyncUnavailable
	}
	if cfg == nil {
		return nil, errors.New("weekly quota sync config is required")
	}
	next := *cfg
	normalizeUserWeeklyQuotaSyncConfig(&next)
	if err := validateUserWeeklyQuotaSyncConfig(&next); err != nil {
		return nil, err
	}
	if next.Enabled {
		if err := s.validateSourceAccount(ctx, next.SourceAccountID); err != nil {
			return nil, err
		}
	}
	previous, err := s.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	if err := s.settingRepo.Set(ctx, SettingKeyUserWeeklyQuotaSyncConfig, string(raw)); err != nil {
		return nil, err
	}
	if previous.SourceAccountID != next.SourceAccountID {
		state := UserWeeklyQuotaSyncState{SourceAccountID: next.SourceAccountID}
		if err := s.saveState(ctx, &state); err != nil {
			return nil, err
		}
	}
	return s.GetStatus(ctx)
}

func (s *UserWeeklyQuotaSyncService) ListSourceAccounts(ctx context.Context) ([]UserWeeklyQuotaSyncAccount, error) {
	if s == nil || s.accountRepo == nil {
		return nil, ErrUserWeeklyQuotaSyncUnavailable
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, err
	}
	result := make([]UserWeeklyQuotaSyncAccount, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if account.Type != AccountTypeOAuth || account.IsShadow() {
			continue
		}
		result = append(result, UserWeeklyQuotaSyncAccount{
			ID:       account.ID,
			Name:     account.Name,
			Status:   account.Status,
			Eligible: isUserWeeklyQuotaSyncSource(account),
		})
	}
	return result, nil
}

func isUserWeeklyQuotaSyncSource(account *Account) bool {
	return account != nil && account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth && !account.IsShadow() && account.IsActive()
}

func (s *UserWeeklyQuotaSyncService) validateSourceAccount(ctx context.Context, accountID int64) error {
	if s == nil || s.accountRepo == nil {
		return ErrUserWeeklyQuotaSyncUnavailable
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil || !isUserWeeklyQuotaSyncSource(account) {
		return ErrUserWeeklyQuotaSyncSourceInvalid
	}
	return nil
}

func (s *UserWeeklyQuotaSyncService) Start() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.wg.Add(1)
	s.mu.Unlock()
	go s.runLoop()
}

func (s *UserWeeklyQuotaSyncService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.parentCancel()
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *UserWeeklyQuotaSyncService) runLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(userWeeklyQuotaSyncTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.parentCtx.Done():
			return
		case <-ticker.C:
			if _, err := s.RunDue(s.parentCtx); err != nil {
				slog.Warn("user_weekly_quota_sync_check_failed", "error", err)
			}
		}
	}
}

// RunDue only acts when enabled and its persisted check cadence is due.
func (s *UserWeeklyQuotaSyncService) RunDue(ctx context.Context) (*UserWeeklyQuotaSyncCheckResult, error) {
	if s == nil {
		return nil, ErrUserWeeklyQuotaSyncUnavailable
	}
	cfg, err := s.GetConfig(ctx)
	if err != nil || !cfg.Enabled {
		return nil, err
	}
	state, err := s.GetState(ctx)
	if err != nil {
		return nil, err
	}
	if state.LastCheckedAt != nil && s.currentTime().Sub(*state.LastCheckedAt) < time.Duration(cfg.PollIntervalSeconds)*time.Second {
		return nil, nil
	}
	return s.runCheck(ctx, cfg)
}

// CheckNow explicitly queries the configured source even when automatic
// monitoring is disabled, allowing an admin to establish a baseline first.
func (s *UserWeeklyQuotaSyncService) CheckNow(ctx context.Context) (*UserWeeklyQuotaSyncCheckResult, error) {
	if s == nil {
		return nil, ErrUserWeeklyQuotaSyncUnavailable
	}
	cfg, err := s.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.SourceAccountID <= 0 {
		return nil, errors.New("source_account_id must be configured before checking")
	}
	return s.runCheck(ctx, cfg)
}

// ResetAllAt provides an explicit bulk anchor operation. It does not alter the
// upstream observation baseline, so the next real upstream reset is still
// detected normally.
func (s *UserWeeklyQuotaSyncService) ResetAllAt(ctx context.Context, start time.Time) (int, error) {
	if s == nil || s.bulkResetter == nil {
		return 0, ErrUserWeeklyQuotaSyncUnavailable
	}
	start = start.UTC().Truncate(time.Second)
	if start.After(s.currentTime().UTC().Truncate(time.Second)) {
		return 0, errors.New("start_at cannot be in the future")
	}
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()
	release, acquired := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db, userWeeklyQuotaSyncLeaderLockKey, s.instanceID, userWeeklyQuotaSyncLeaderLockTTL)
	if !acquired {
		return 0, ErrUserWeeklyQuotaSyncBusy
	}
	defer release()
	return s.resetUsersAt(ctx, start)
}

func (s *UserWeeklyQuotaSyncService) runCheck(ctx context.Context, cfg *UserWeeklyQuotaSyncConfig) (*UserWeeklyQuotaSyncCheckResult, error) {
	if s == nil || s.quotaService == nil || s.bulkResetter == nil {
		return nil, ErrUserWeeklyQuotaSyncUnavailable
	}
	s.cycleMu.Lock()
	defer s.cycleMu.Unlock()

	release, acquired := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db, userWeeklyQuotaSyncLeaderLockKey, s.instanceID, userWeeklyQuotaSyncLeaderLockTTL)
	if !acquired {
		return nil, ErrUserWeeklyQuotaSyncBusy
	}
	defer release()

	now := s.currentTime().UTC().Truncate(time.Second)
	state, err := s.GetState(ctx)
	if err != nil {
		return nil, err
	}
	sourceChanged := state.SourceAccountID != cfg.SourceAccountID
	state.SourceAccountID = cfg.SourceAccountID
	state.LastCheckedAt = &now

	if err := s.validateSourceAccount(ctx, cfg.SourceAccountID); err != nil {
		state.LastError = err.Error()
		_ = s.saveState(ctx, state)
		return nil, err
	}
	usage, err := s.quotaService.QueryUsage(ctx, cfg.SourceAccountID)
	if err != nil {
		state.LastError = err.Error()
		_ = s.saveState(ctx, state)
		return nil, err
	}
	weekly, err := findOpenAIWeeklyQuotaWindow(usage)
	if err != nil {
		state.LastError = err.Error()
		_ = s.saveState(ctx, state)
		return nil, err
	}
	resetAt := time.Unix(weekly.ResetAt, 0).UTC()
	windowStart := resetAt.Add(-time.Duration(weekly.LimitWindowSeconds) * time.Second).Truncate(time.Second)
	if windowStart.After(now.Add(5 * time.Minute)) {
		err := fmt.Errorf("upstream seven-day reset time is invalid")
		state.LastError = err.Error()
		_ = s.saveState(ctx, state)
		return nil, err
	}

	result := &UserWeeklyQuotaSyncCheckResult{ResetAt: resetAt, WindowStart: windowStart}
	if state.ObservedResetAt == nil || sourceChanged {
		state.ObservedResetAt = &resetAt
		state.ObservedWindowSeconds = weekly.LimitWindowSeconds
		state.PendingResetAt = nil
		state.LastError = ""
		if err := s.saveState(ctx, state); err != nil {
			return nil, err
		}
		result.BaselineInitialized = true
		result.Status = UserWeeklyQuotaSyncStatus{Config: *cfg, State: *state}
		return result, nil
	}

	if upstreamWeeklyWindowAdvanced(*state.ObservedResetAt, resetAt) {
		// A reset boundary is destructive for every configured user. Require a
		// second consecutive observation of the exact same new reset_at before
		// applying it. This still accepts early official resets: there is no
		// "half of a seven-day window" threshold here.
		if state.PendingResetAt == nil || !resetAt.Equal(*state.PendingResetAt) {
			state.PendingResetAt = &resetAt
			state.LastError = ""
			if err := s.saveState(ctx, state); err != nil {
				return nil, err
			}
			result.AwaitingConfirmation = true
			result.Status = UserWeeklyQuotaSyncStatus{Config: *cfg, State: *state}
			return result, nil
		}

		affected, resetErr := s.resetUsersAt(ctx, windowStart)
		if resetErr != nil {
			state.LastError = resetErr.Error()
			_ = s.saveState(ctx, state)
			return nil, resetErr
		}
		state.LastTriggeredAt = &now
		state.LastWindowStart = &windowStart
		state.LastAffectedUsers = affected
		result.ResetDetected = true
		result.AffectedUsers = affected
	}

	// Do not replace the last confirmed observation with a backward timestamp:
	// an inconsistent upstream response must never make a later real reset look
	// old. An equal timestamp is still allowed to refresh the window metadata.
	if !resetAt.Before(*state.ObservedResetAt) {
		state.ObservedResetAt = &resetAt
		state.ObservedWindowSeconds = weekly.LimitWindowSeconds
	}
	state.PendingResetAt = nil
	state.LastError = ""
	if err := s.saveState(ctx, state); err != nil {
		return nil, err
	}
	result.Status = UserWeeklyQuotaSyncStatus{Config: *cfg, State: *state}
	return result, nil
}

func (s *UserWeeklyQuotaSyncService) resetUsersAt(ctx context.Context, start time.Time) (int, error) {
	userIDs, err := s.bulkResetter.ResetWeeklyWindowForPlatform(ctx, PlatformOpenAI, start)
	if err != nil {
		return 0, err
	}
	if s.cacheResetter != nil {
		if err := s.cacheResetter.ResetUserPlatformQuotaWeeklyCache(ctx, userIDs, PlatformOpenAI, start); err != nil {
			slog.Error("user_weekly_quota_sync_cache_reset_failed", "affected_users", len(userIDs), "error", err)
		}
	}
	return len(userIDs), nil
}

func (s *UserWeeklyQuotaSyncService) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func upstreamWeeklyWindowAdvanced(previous, current time.Time) bool {
	return current.After(previous)
}

func findOpenAIWeeklyQuotaWindow(usage *OpenAIQuotaUsage) (*OpenAIRateLimitWindow, error) {
	if usage == nil {
		return nil, ErrUserWeeklyQuotaSyncNoWeeklyLimit
	}
	type candidate struct {
		window   *OpenAIRateLimitWindow
		priority int
		distance time.Duration
	}
	candidates := make([]candidate, 0, 8)
	add := func(limit *OpenAIRateLimit, priority int) {
		if limit == nil {
			return
		}
		for _, window := range []*OpenAIRateLimitWindow{limit.PrimaryWindow, limit.SecondaryWindow} {
			if window == nil || window.ResetAt <= 0 || window.LimitWindowSeconds <= 0 {
				continue
			}
			duration := time.Duration(window.LimitWindowSeconds) * time.Second
			distance := duration - userWeeklyQuotaSyncTargetWindow
			if distance < 0 {
				distance = -distance
			}
			if distance > userWeeklyQuotaSyncWindowTolerance {
				continue
			}
			candidates = append(candidates, candidate{window: window, priority: priority, distance: distance})
		}
	}
	// A Codex-specific additional limit is the best signal for the requested
	// Codex account, but retain the base account rate limit as a fallback.
	for _, additional := range usage.AdditionalRateLimits {
		priority := 2
		if strings.Contains(strings.ToLower(additional.MeteredFeature+" "+additional.LimitName), "codex") {
			priority = 0
		}
		add(additional.RateLimit, priority)
	}
	add(usage.RateLimit, 1)
	if len(candidates) == 0 {
		return nil, ErrUserWeeklyQuotaSyncNoWeeklyLimit
	}
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.priority < best.priority || (candidate.priority == best.priority && candidate.distance < best.distance) ||
			(candidate.priority == best.priority && candidate.distance == best.distance && candidate.window.ResetAt > best.window.ResetAt) {
			best = candidate
		}
	}
	return best.window, nil
}
