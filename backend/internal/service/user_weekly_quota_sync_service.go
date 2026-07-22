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
	// The percentage detector is a fallback for the period immediately after a
	// Codex reset, when /wham/usage may deliberately omit reset_at and
	// reset_after_seconds. Requiring a meaningful prior peak avoids treating a
	// rounding correction near zero as a reset for every user.
	userWeeklyQuotaSyncUsageResetMinimumPercent = 1.0
	userWeeklyQuotaSyncUsageZeroPercent         = 0.01
	// The upstream countdown is rounded while a request is in flight. Treat
	// small differences as the same boundary so ordinary second-level drift is
	// never mistaken for an early official reset.
	userWeeklyQuotaSyncBoundaryTolerance = 2 * time.Minute
	// The account quota card uses the live Codex countdown. If reset_after_seconds
	// disagrees materially with reset_at, use the countdown so synchronization
	// follows the same real Codex window shown to the admin.
	userWeeklyQuotaSyncResetAtTolerance  = 2 * time.Minute
	userWeeklyQuotaSyncObservationSource = "codex_7d_usage_and_reset_v2"

	userWeeklyQuotaSyncSignalResetTime    = "reset_time"
	userWeeklyQuotaSyncSignalUsagePercent = "usage_percent_zero"
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
	// ObservedWeeklyUsedPercent is the latest 7-day Codex usage percentage.
	// Unlike reset_at, it remains meaningful while a just-reset account has no
	// next-reset timestamp yet.
	ObservedWeeklyUsedPercent *float64 `json:"observed_weekly_used_percent,omitempty"`
	// PeakWeeklyUsedPercent is reset after a confirmed upstream reset. It lets
	// the percentage detector require a real, non-zero usage baseline before a
	// later 0% observation can affect every user's quota.
	PeakWeeklyUsedPercent *float64 `json:"peak_weekly_used_percent,omitempty"`
	// ObservedSource identifies the upstream signal used for the persisted
	// baseline. Changing it deliberately starts a new baseline rather than
	// treating a different quota window as a user-quota reset.
	ObservedSource string `json:"observed_source,omitempty"`
	// PendingSignal and its fields persist a candidate reset until a second
	// observation confirms it. For a percentage signal PendingResetAt is empty,
	// because Codex has not supplied a next-reset timestamp yet; the first 0%
	// observation time is retained as the user-quota window anchor instead.
	PendingSignal      string     `json:"pending_signal,omitempty"`
	PendingResetAt     *time.Time `json:"pending_reset_at,omitempty"`
	PendingWindowStart *time.Time `json:"pending_window_start,omitempty"`
	LastCheckedAt      *time.Time `json:"last_checked_at,omitempty"`
	LastTriggeredAt    *time.Time `json:"last_triggered_at,omitempty"`
	LastWindowStart    *time.Time `json:"last_window_start,omitempty"`
	LastAffectedUsers  int        `json:"last_affected_users"`
	LastTriggerSignal  string     `json:"last_trigger_signal,omitempty"`
	LastError          string     `json:"last_error,omitempty"`
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
	DetectionSignal      string                    `json:"detection_signal,omitempty"`
	ResetAt              *time.Time                `json:"reset_at,omitempty"`
	WindowStart          *time.Time                `json:"window_start,omitempty"`
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
	observation, err := findOpenAIWeeklyQuotaObservation(usage, now)
	if err != nil {
		state.LastError = err.Error()
		_ = s.saveState(ctx, state)
		return nil, err
	}

	// A just-reset Codex account can expose a valid 7-day window and 0% usage
	// while omitting reset_at/reset_after_seconds. Ignore an impossible future
	// timestamp and continue with the percentage detector instead of failing the
	// whole synchronization check.
	if observation.resetAt != nil {
		windowStart := observation.resetAt.Add(-time.Duration(observation.windowSeconds) * time.Second).Truncate(time.Second)
		if windowStart.After(now.Add(5 * time.Minute)) {
			observation.resetAt = nil
		}
	}

	result := &UserWeeklyQuotaSyncCheckResult{}
	setWeeklyQuotaSyncCheckResultObservation(result, observation)
	if sourceChanged || state.ObservedSource != observation.source {
		setWeeklyQuotaSyncObservationBaseline(state, observation)
		state.LastError = ""
		if err := s.saveState(ctx, state); err != nil {
			return nil, err
		}
		result.BaselineInitialized = true
		result.Status = UserWeeklyQuotaSyncStatus{Config: *cfg, State: *state}
		return result, nil
	}

	candidate := findWeeklyQuotaSyncResetCandidate(state, observation, now)
	if candidate != nil {
		result.DetectionSignal = candidate.signal
		result.ResetAt = weeklyQuotaSyncTimePtrFromPtr(candidate.resetAt)
		result.WindowStart = weeklyQuotaSyncTimePtr(candidate.windowStart)

		// A reset is destructive for every configured user. Both detectors need
		// two consecutive observations: an advancing upstream boundary, or a
		// meaningful prior usage percentage followed by 0% twice.
		if !weeklyQuotaSyncPendingCandidateConfirmed(state, candidate) {
			setWeeklyQuotaSyncPendingCandidate(state, candidate)
			state.LastError = ""
			if err := s.saveState(ctx, state); err != nil {
				return nil, err
			}
			result.AwaitingConfirmation = true
			result.Status = UserWeeklyQuotaSyncStatus{Config: *cfg, State: *state}
			return result, nil
		}

		windowStart := weeklyQuotaSyncPendingWindowStart(state, candidate)
		affected, resetErr := s.resetUsersAt(ctx, windowStart)
		if resetErr != nil {
			state.LastError = resetErr.Error()
			_ = s.saveState(ctx, state)
			return nil, resetErr
		}
		state.LastTriggeredAt = &now
		state.LastWindowStart = &windowStart
		state.LastAffectedUsers = affected
		state.LastTriggerSignal = candidate.signal
		setWeeklyQuotaSyncConfirmedObservation(state, observation, candidate)
		result.ResetDetected = true
		result.AffectedUsers = affected
		result.WindowStart = weeklyQuotaSyncTimePtr(windowStart)
		state.LastError = ""
		if err := s.saveState(ctx, state); err != nil {
			return nil, err
		}
		result.Status = UserWeeklyQuotaSyncStatus{Config: *cfg, State: *state}
		return result, nil
	}

	clearWeeklyQuotaSyncPendingCandidate(state)
	refreshWeeklyQuotaSyncObservation(state, observation)
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
	return current.After(previous.Add(userWeeklyQuotaSyncBoundaryTolerance))
}

func sameUpstreamWeeklyBoundary(left, right time.Time) bool {
	delta := left.Sub(right)
	if delta < 0 {
		delta = -delta
	}
	return delta <= userWeeklyQuotaSyncBoundaryTolerance
}

type weeklyQuotaSyncResetCandidate struct {
	signal      string
	resetAt     *time.Time
	windowStart time.Time
}

func findWeeklyQuotaSyncResetCandidate(state *UserWeeklyQuotaSyncState, observation *openAIWeeklyQuotaObservation, now time.Time) *weeklyQuotaSyncResetCandidate {
	if state == nil || observation == nil {
		return nil
	}
	if observation.resetAt != nil && state.ObservedResetAt != nil && upstreamWeeklyWindowAdvanced(*state.ObservedResetAt, *observation.resetAt) {
		start := observation.resetAt.Add(-time.Duration(observation.windowSeconds) * time.Second).Truncate(time.Second)
		return &weeklyQuotaSyncResetCandidate{
			signal:      userWeeklyQuotaSyncSignalResetTime,
			resetAt:     weeklyQuotaSyncTimePtr(*observation.resetAt),
			windowStart: start,
		}
	}
	if state.PeakWeeklyUsedPercent != nil &&
		*state.PeakWeeklyUsedPercent >= userWeeklyQuotaSyncUsageResetMinimumPercent &&
		observation.usedPercent <= userWeeklyQuotaSyncUsageZeroPercent {
		return &weeklyQuotaSyncResetCandidate{
			signal:      userWeeklyQuotaSyncSignalUsagePercent,
			windowStart: now.UTC().Truncate(time.Second),
		}
	}
	return nil
}

func weeklyQuotaSyncPendingCandidateConfirmed(state *UserWeeklyQuotaSyncState, candidate *weeklyQuotaSyncResetCandidate) bool {
	if state == nil || candidate == nil || state.PendingSignal != candidate.signal || state.PendingWindowStart == nil {
		return false
	}
	if candidate.signal != userWeeklyQuotaSyncSignalResetTime {
		return candidate.signal == userWeeklyQuotaSyncSignalUsagePercent
	}
	return state.PendingResetAt != nil && candidate.resetAt != nil && sameUpstreamWeeklyBoundary(*state.PendingResetAt, *candidate.resetAt)
}

func setWeeklyQuotaSyncPendingCandidate(state *UserWeeklyQuotaSyncState, candidate *weeklyQuotaSyncResetCandidate) {
	if state == nil || candidate == nil {
		return
	}
	state.PendingSignal = candidate.signal
	state.PendingResetAt = weeklyQuotaSyncTimePtrFromPtr(candidate.resetAt)
	state.PendingWindowStart = weeklyQuotaSyncTimePtr(candidate.windowStart)
}

func clearWeeklyQuotaSyncPendingCandidate(state *UserWeeklyQuotaSyncState) {
	if state == nil {
		return
	}
	state.PendingSignal = ""
	state.PendingResetAt = nil
	state.PendingWindowStart = nil
}

func weeklyQuotaSyncPendingWindowStart(state *UserWeeklyQuotaSyncState, candidate *weeklyQuotaSyncResetCandidate) time.Time {
	if state != nil && state.PendingWindowStart != nil {
		return state.PendingWindowStart.UTC().Truncate(time.Second)
	}
	if candidate != nil {
		return candidate.windowStart.UTC().Truncate(time.Second)
	}
	return time.Now().UTC().Truncate(time.Second)
}

func setWeeklyQuotaSyncObservationBaseline(state *UserWeeklyQuotaSyncState, observation *openAIWeeklyQuotaObservation) {
	if state == nil || observation == nil {
		return
	}
	state.ObservedResetAt = weeklyQuotaSyncTimePtrFromPtr(observation.resetAt)
	state.ObservedWindowSeconds = observation.windowSeconds
	state.ObservedWeeklyUsedPercent = weeklyQuotaSyncFloat64Ptr(observation.usedPercent)
	state.PeakWeeklyUsedPercent = weeklyQuotaSyncFloat64Ptr(observation.usedPercent)
	state.ObservedSource = observation.source
	clearWeeklyQuotaSyncPendingCandidate(state)
}

func refreshWeeklyQuotaSyncObservation(state *UserWeeklyQuotaSyncState, observation *openAIWeeklyQuotaObservation) {
	if state == nil || observation == nil {
		return
	}
	// Do not move an established boundary backwards because an intermittent
	// upstream response must not make a later real reset appear old. A missing
	// reset time is intentionally ignored here; it is normal at 0% usage.
	if observation.resetAt != nil && (state.ObservedResetAt == nil || !observation.resetAt.Before(*state.ObservedResetAt) || sameUpstreamWeeklyBoundary(*observation.resetAt, *state.ObservedResetAt)) {
		state.ObservedResetAt = weeklyQuotaSyncTimePtrFromPtr(observation.resetAt)
	}
	state.ObservedWindowSeconds = observation.windowSeconds
	state.ObservedWeeklyUsedPercent = weeklyQuotaSyncFloat64Ptr(observation.usedPercent)
	if state.PeakWeeklyUsedPercent == nil || observation.usedPercent > *state.PeakWeeklyUsedPercent {
		state.PeakWeeklyUsedPercent = weeklyQuotaSyncFloat64Ptr(observation.usedPercent)
	}
	state.ObservedSource = observation.source
}

func setWeeklyQuotaSyncConfirmedObservation(state *UserWeeklyQuotaSyncState, observation *openAIWeeklyQuotaObservation, candidate *weeklyQuotaSyncResetCandidate) {
	if state == nil || observation == nil || candidate == nil {
		return
	}
	// A percentage-confirmed reset has no trustworthy upstream reset boundary.
	// Clear the old one so a reset_at that reappears after 1-5% new-cycle usage
	// is merely established as a new baseline, never interpreted as another
	// reset of the users' weekly quota.
	if candidate.signal == userWeeklyQuotaSyncSignalResetTime {
		state.ObservedResetAt = weeklyQuotaSyncTimePtrFromPtr(candidate.resetAt)
	} else {
		state.ObservedResetAt = nil
	}
	state.ObservedWindowSeconds = observation.windowSeconds
	state.ObservedWeeklyUsedPercent = weeklyQuotaSyncFloat64Ptr(observation.usedPercent)
	state.PeakWeeklyUsedPercent = weeklyQuotaSyncFloat64Ptr(observation.usedPercent)
	state.ObservedSource = observation.source
	clearWeeklyQuotaSyncPendingCandidate(state)
}

func setWeeklyQuotaSyncCheckResultObservation(result *UserWeeklyQuotaSyncCheckResult, observation *openAIWeeklyQuotaObservation) {
	if result == nil || observation == nil || observation.resetAt == nil {
		return
	}
	start := observation.resetAt.Add(-time.Duration(observation.windowSeconds) * time.Second).Truncate(time.Second)
	result.ResetAt = weeklyQuotaSyncTimePtr(*observation.resetAt)
	result.WindowStart = weeklyQuotaSyncTimePtr(start)
}

func weeklyQuotaSyncTimePtr(value time.Time) *time.Time {
	return &value
}

func weeklyQuotaSyncTimePtrFromPtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return weeklyQuotaSyncTimePtr(*value)
}

func weeklyQuotaSyncFloat64Ptr(value float64) *float64 {
	return &value
}

type openAIWeeklyQuotaSignal struct {
	resetAt       time.Time
	windowSeconds int64
	source        string
}

type openAIWeeklyQuotaObservation struct {
	usedPercent   float64
	resetAt       *time.Time
	windowSeconds int64
	source        string
}

// findOpenAIWeeklyQuotaObservation reads the Codex seven-day window even when
// its next-reset fields are absent. Codex returns exactly that shape after some
// official resets: used_percent is 0 while reset_at/reset_after_seconds are
// empty until the account spends a little quota in the new window.
func findOpenAIWeeklyQuotaObservation(usage *OpenAIQuotaUsage, fallbackNow time.Time) (*openAIWeeklyQuotaObservation, error) {
	weekly, err := findOpenAIWeeklyQuotaWindow(usage)
	if err != nil {
		return nil, err
	}

	observedAt := fallbackNow.UTC().Truncate(time.Second)
	if usage.FetchedAt > 0 {
		observedAt = time.Unix(usage.FetchedAt, 0).UTC()
	}

	var resetAt time.Time
	if weekly.ResetAt > 0 {
		resetAt = time.Unix(weekly.ResetAt, 0).UTC()
	}
	if weekly.ResetAfterSeconds > 0 {
		fromCountdown := observedAt.Add(time.Duration(weekly.ResetAfterSeconds) * time.Second).Truncate(time.Second)
		if resetAt.IsZero() || !sameWeeklyResetTime(resetAt, fromCountdown) {
			resetAt = fromCountdown
		}
	}
	var resetAtPtr *time.Time
	if !resetAt.IsZero() {
		resetAtPtr = weeklyQuotaSyncTimePtr(resetAt.UTC().Truncate(time.Second))
	}

	return &openAIWeeklyQuotaObservation{
		usedPercent:   weekly.UsedPercent,
		resetAt:       resetAtPtr,
		windowSeconds: weekly.LimitWindowSeconds,
		source:        userWeeklyQuotaSyncObservationSource,
	}, nil
}

// findOpenAIWeeklyQuotaSignal is retained for callers and focused tests that
// specifically need a next-reset timestamp. The synchronizer itself uses the
// broader observation above so 0%-usage responses do not abort the check.
func findOpenAIWeeklyQuotaSignal(usage *OpenAIQuotaUsage, fallbackNow time.Time) (*openAIWeeklyQuotaSignal, error) {
	observation, err := findOpenAIWeeklyQuotaObservation(usage, fallbackNow)
	if err != nil {
		return nil, err
	}
	if observation.resetAt == nil {
		return nil, ErrUserWeeklyQuotaSyncNoWeeklyLimit
	}
	return &openAIWeeklyQuotaSignal{
		resetAt:       *observation.resetAt,
		windowSeconds: observation.windowSeconds,
		source:        observation.source,
	}, nil
}

func sameWeeklyResetTime(left, right time.Time) bool {
	delta := left.Sub(right)
	if delta < 0 {
		delta = -delta
	}
	return delta <= userWeeklyQuotaSyncResetAtTolerance
}

func findOpenAIWeeklyQuotaWindow(usage *OpenAIQuotaUsage) (*OpenAIRateLimitWindow, error) {
	if usage == nil {
		return nil, ErrUserWeeklyQuotaSyncNoWeeklyLimit
	}
	type candidate struct {
		window   *OpenAIRateLimitWindow
		distance time.Duration
	}
	candidates := make([]candidate, 0, 2)
	add := func(limit *OpenAIRateLimit) {
		if limit == nil {
			return
		}
		for _, window := range []*OpenAIRateLimitWindow{limit.PrimaryWindow, limit.SecondaryWindow} {
			// A 7-day window may intentionally omit its reset fields immediately
			// after an official reset. Its usage percentage is still the fallback
			// synchronization signal, so do not discard it here.
			if window == nil || window.LimitWindowSeconds <= 0 {
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
			candidates = append(candidates, candidate{window: window, distance: distance})
		}
	}
	// The primary rate_limit belongs to the Codex request made by QueryUsage and
	// is the same quota family represented by the source account card. Additional
	// limits can represent independent products, so never prefer them merely
	// because their names contain "codex". Keep one exact fallback for accounts
	// where the primary envelope has no seven-day window.
	add(usage.RateLimit)
	if len(candidates) == 0 {
		for _, additional := range usage.AdditionalRateLimits {
			if strings.EqualFold(strings.TrimSpace(additional.MeteredFeature), "codex_bengalfox") {
				add(additional.RateLimit)
				break
			}
		}
	}
	if len(candidates) == 0 {
		return nil, ErrUserWeeklyQuotaSyncNoWeeklyLimit
	}
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.distance < best.distance ||
			(candidate.distance == best.distance && candidate.window.ResetAt > best.window.ResetAt) {
			best = candidate
		}
	}
	return best.window, nil
}
