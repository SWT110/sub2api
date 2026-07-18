package service

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type weeklyQuotaSyncSettingRepo struct {
	SettingRepository
	values map[string]string
}

func (r *weeklyQuotaSyncSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *weeklyQuotaSyncSettingRepo) Set(_ context.Context, key, value string) error {
	r.values[key] = value
	return nil
}

type weeklyQuotaSyncAccountRepo struct {
	AccountRepository
	accounts map[int64]*Account
}

func (r *weeklyQuotaSyncAccountRepo) GetByID(_ context.Context, id int64) (*Account, error) {
	return r.accounts[id], nil
}

func (r *weeklyQuotaSyncAccountRepo) ListByPlatform(_ context.Context, _ string) ([]Account, error) {
	result := make([]Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		result = append(result, *account)
	}
	return result, nil
}

type weeklyQuotaSyncUsageReader struct {
	responses []*OpenAIQuotaUsage
	index     int
}

func (r *weeklyQuotaSyncUsageReader) QueryUsage(_ context.Context, _ int64) (*OpenAIQuotaUsage, error) {
	if r.index >= len(r.responses) {
		return r.responses[len(r.responses)-1], nil
	}
	result := r.responses[r.index]
	r.index++
	return result, nil
}

type weeklyQuotaSyncResetter struct {
	starts []time.Time
	ids    []int64
}

func (r *weeklyQuotaSyncResetter) ResetWeeklyWindowForPlatform(_ context.Context, platform string, start time.Time) ([]int64, error) {
	if platform != PlatformOpenAI {
		return nil, ErrUserWeeklyQuotaSyncUnavailable
	}
	r.starts = append(r.starts, start)
	return append([]int64(nil), r.ids...), nil
}

type weeklyQuotaSyncCacheResetter struct {
	calls int
	ids   []int64
	start time.Time
}

func (r *weeklyQuotaSyncCacheResetter) ResetUserPlatformQuotaWeeklyCache(_ context.Context, userIDs []int64, platform string, start time.Time) error {
	if platform != PlatformOpenAI {
		return ErrUserWeeklyQuotaSyncUnavailable
	}
	r.calls++
	r.ids = append([]int64(nil), userIDs...)
	r.start = start
	return nil
}

func weeklyUsage(resetAt time.Time) *OpenAIQuotaUsage {
	return &OpenAIQuotaUsage{
		RateLimit: &OpenAIRateLimit{
			PrimaryWindow:   &OpenAIRateLimitWindow{LimitWindowSeconds: 5 * 60 * 60, ResetAt: resetAt.Add(-5 * time.Hour).Unix()},
			SecondaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: int64((7 * 24 * time.Hour).Seconds()), ResetAt: resetAt.Unix()},
		},
	}
}

func TestFindOpenAIWeeklyQuotaWindowPrefersCodexAdditionalLimit(t *testing.T) {
	baseReset := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	codexReset := baseReset.Add(time.Hour)
	usage := &OpenAIQuotaUsage{
		RateLimit: &OpenAIRateLimit{
			PrimaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: int64((7 * 24 * time.Hour).Seconds()), ResetAt: baseReset.Unix()},
		},
		AdditionalRateLimits: []OpenAIAdditionalRateLimit{{
			MeteredFeature: "codex_bengalfox",
			RateLimit: &OpenAIRateLimit{SecondaryWindow: &OpenAIRateLimitWindow{
				LimitWindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
				ResetAt:            codexReset.Unix(),
			}},
		}},
	}

	window, err := findOpenAIWeeklyQuotaWindow(usage)
	require.NoError(t, err)
	require.Equal(t, codexReset.Unix(), window.ResetAt)
}

func TestUserWeeklyQuotaSyncCheckBaselinesThenResetsUsers(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	firstReset := now.Add(3 * 24 * time.Hour)
	secondReset := firstReset.Add(7 * 24 * time.Hour)
	config := UserWeeklyQuotaSyncConfig{Enabled: true, SourceAccountID: 9, PollIntervalSeconds: 60}
	configJSON, err := json.Marshal(config)
	require.NoError(t, err)
	settings := &weeklyQuotaSyncSettingRepo{values: map[string]string{
		SettingKeyUserWeeklyQuotaSyncConfig: string(configJSON),
	}}
	accounts := &weeklyQuotaSyncAccountRepo{accounts: map[int64]*Account{
		9: {ID: 9, Name: "Codex Pro", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive},
	}}
	resetter := &weeklyQuotaSyncResetter{ids: []int64{11, 12}}
	cache := &weeklyQuotaSyncCacheResetter{}
	service := &UserWeeklyQuotaSyncService{
		settingRepo:   settings,
		accountRepo:   accounts,
		quotaService:  &weeklyQuotaSyncUsageReader{responses: []*OpenAIQuotaUsage{weeklyUsage(firstReset), weeklyUsage(secondReset)}},
		bulkResetter:  resetter,
		cacheResetter: cache,
		instanceID:    "test-instance",
		now:           func() time.Time { return now },
	}

	baseline, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, baseline.BaselineInitialized)
	require.False(t, baseline.ResetDetected)
	require.Empty(t, resetter.starts)

	now = firstReset.Add(10 * time.Minute)
	triggered, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, triggered.ResetDetected)
	require.Equal(t, 2, triggered.AffectedUsers)
	require.Len(t, resetter.starts, 1)
	wantStart := secondReset.Add(-7 * 24 * time.Hour)
	require.Equal(t, wantStart, resetter.starts[0])
	require.Equal(t, 1, cache.calls)
	require.Equal(t, []int64{11, 12}, cache.ids)
	require.Equal(t, wantStart, cache.start)
}

func TestUserWeeklyQuotaSyncResetAllAtResetsOnlyAffectedUsers(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 7, 20, 12, 34, 56, 987654321, time.FixedZone("UTC+8", 8*60*60))
	resetter := &weeklyQuotaSyncResetter{ids: []int64{21, 22, 23}}
	cache := &weeklyQuotaSyncCacheResetter{}
	service := &UserWeeklyQuotaSyncService{
		bulkResetter:  resetter,
		cacheResetter: cache,
		instanceID:    "test-instance",
		now:           func() time.Time { return start.Add(time.Minute) },
	}

	affected, err := service.ResetAllAt(ctx, start)
	require.NoError(t, err)
	require.Equal(t, 3, affected)
	require.Equal(t, []time.Time{start.UTC().Truncate(time.Second)}, resetter.starts)
	require.Equal(t, 1, cache.calls)
	require.Equal(t, []int64{21, 22, 23}, cache.ids)
	require.Equal(t, start.UTC().Truncate(time.Second), cache.start)
}

func TestUserWeeklyQuotaSyncResetAllAtRejectsFutureStart(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	resetter := &weeklyQuotaSyncResetter{ids: []int64{21}}
	service := &UserWeeklyQuotaSyncService{
		bulkResetter: resetter,
		instanceID:   "test-instance",
		now:          func() time.Time { return now },
	}

	affected, err := service.ResetAllAt(context.Background(), now.Add(time.Second))
	require.Error(t, err)
	require.Zero(t, affected)
	require.Empty(t, resetter.starts)
}
