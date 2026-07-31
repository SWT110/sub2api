package service

import (
	"context"
	"encoding/json"
	"errors"
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
	calls              int
	ids                []int64
	start              time.Time
	alignCalls         int
	alignIDs           []int64
	alignExpectedStart time.Time
	alignNewStart      time.Time
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

func (r *weeklyQuotaSyncCacheResetter) AlignUserPlatformQuotaWeeklyCache(_ context.Context, userIDs []int64, platform string, expectedStart, newStart time.Time) error {
	if platform != PlatformOpenAI {
		return ErrUserWeeklyQuotaSyncUnavailable
	}
	r.alignCalls++
	r.alignIDs = append([]int64(nil), userIDs...)
	r.alignExpectedStart = expectedStart
	r.alignNewStart = newStart
	return nil
}

type weeklyQuotaSyncWindowAlignCall struct {
	expectedStart time.Time
	newStart      time.Time
}

type weeklyQuotaSyncWindowAligner struct {
	calls []weeklyQuotaSyncWindowAlignCall
	ids   []int64
}

func (r *weeklyQuotaSyncWindowAligner) AlignWeeklyWindowStartForPlatform(_ context.Context, platform string, expectedStart, newStart time.Time) ([]int64, error) {
	if platform != PlatformOpenAI {
		return nil, ErrUserWeeklyQuotaSyncUnavailable
	}
	r.calls = append(r.calls, weeklyQuotaSyncWindowAlignCall{expectedStart: expectedStart, newStart: newStart})
	return append([]int64(nil), r.ids...), nil
}

type weeklyQuotaSyncRateLimitClearer struct {
	calls []int64
	errs  []error
}

func (r *weeklyQuotaSyncRateLimitClearer) ClearRateLimit(_ context.Context, accountID int64) error {
	callIndex := len(r.calls)
	r.calls = append(r.calls, accountID)
	if callIndex < len(r.errs) {
		return r.errs[callIndex]
	}
	return nil
}

func newUserWeeklyQuotaSyncServiceForTest(
	t *testing.T,
	now *time.Time,
	responses []*OpenAIQuotaUsage,
	resetter *weeklyQuotaSyncResetter,
	aligner *weeklyQuotaSyncWindowAligner,
	cache *weeklyQuotaSyncCacheResetter,
	clearer sourceAccountRateLimitClearer,
) *UserWeeklyQuotaSyncService {
	t.Helper()
	config := UserWeeklyQuotaSyncConfig{Enabled: true, SourceAccountID: 9, PollIntervalSeconds: 60}
	configJSON, err := json.Marshal(config)
	require.NoError(t, err)
	service := &UserWeeklyQuotaSyncService{
		settingRepo: &weeklyQuotaSyncSettingRepo{values: map[string]string{
			SettingKeyUserWeeklyQuotaSyncConfig: string(configJSON),
		}},
		accountRepo: &weeklyQuotaSyncAccountRepo{accounts: map[int64]*Account{
			9: {ID: 9, Name: "Codex Pro", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive},
		}},
		quotaService:     &weeklyQuotaSyncUsageReader{responses: responses},
		bulkResetter:     resetter,
		rateLimitClearer: clearer,
		instanceID:       "test-instance",
		now:              func() time.Time { return *now },
	}
	if aligner != nil {
		service.windowAligner = aligner
	}
	if cache != nil {
		service.cacheResetter = cache
		service.cacheAligner = cache
	}
	return service
}

func weeklyUsage(resetAt time.Time) *OpenAIQuotaUsage {
	return &OpenAIQuotaUsage{
		RateLimit: &OpenAIRateLimit{
			PrimaryWindow:   &OpenAIRateLimitWindow{LimitWindowSeconds: 5 * 60 * 60, ResetAt: resetAt.Add(-5 * time.Hour).Unix()},
			SecondaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: int64((7 * 24 * time.Hour).Seconds()), ResetAt: resetAt.Unix()},
		},
	}
}

func weeklyUsageWithPercent(usedPercent float64, resetAt *time.Time) *OpenAIQuotaUsage {
	weekly := &OpenAIRateLimitWindow{
		UsedPercent:        usedPercent,
		LimitWindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
	}
	if resetAt != nil {
		weekly.ResetAt = resetAt.Unix()
	}
	return &OpenAIQuotaUsage{
		RateLimit: &OpenAIRateLimit{
			PrimaryWindow:   &OpenAIRateLimitWindow{LimitWindowSeconds: 5 * 60 * 60},
			SecondaryWindow: weekly,
		},
	}
}

func TestFindOpenAIWeeklyQuotaWindowUsesPrimaryBeforeIndependentAdditionalLimit(t *testing.T) {
	baseReset := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	additionalReset := baseReset.Add(time.Hour)
	usage := &OpenAIQuotaUsage{
		RateLimit: &OpenAIRateLimit{
			PrimaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: int64((7 * 24 * time.Hour).Seconds()), ResetAt: baseReset.Unix()},
		},
		AdditionalRateLimits: []OpenAIAdditionalRateLimit{{
			MeteredFeature: "codex_bengalfox",
			RateLimit: &OpenAIRateLimit{SecondaryWindow: &OpenAIRateLimitWindow{
				LimitWindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
				ResetAt:            additionalReset.Unix(),
			}},
		}},
	}

	window, err := findOpenAIWeeklyQuotaWindow(usage)
	require.NoError(t, err)
	require.Equal(t, baseReset.Unix(), window.ResetAt)
}

func TestFindOpenAIWeeklyQuotaWindowFallsBackToBengalfox(t *testing.T) {
	resetAt := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	usage := &OpenAIQuotaUsage{
		RateLimit: &OpenAIRateLimit{
			PrimaryWindow: &OpenAIRateLimitWindow{LimitWindowSeconds: 5 * 60 * 60, ResetAt: resetAt.Unix()},
		},
		AdditionalRateLimits: []OpenAIAdditionalRateLimit{{
			MeteredFeature: "codex_bengalfox",
			RateLimit: &OpenAIRateLimit{SecondaryWindow: &OpenAIRateLimitWindow{
				LimitWindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
				ResetAt:            resetAt.Unix(),
			}},
		}},
	}

	window, err := findOpenAIWeeklyQuotaWindow(usage)
	require.NoError(t, err)
	require.Equal(t, resetAt.Unix(), window.ResetAt)
}

func TestFindOpenAIWeeklyQuotaSignalUsesCountdownWhenResetAtDisagrees(t *testing.T) {
	fetchedAt := time.Date(2026, 7, 18, 12, 59, 23, 0, time.UTC)
	wantReset := fetchedAt.Add(6*24*time.Hour + 14*time.Hour + 25*time.Minute + 38*time.Second)
	usage := &OpenAIQuotaUsage{
		FetchedAt: fetchedAt.Unix(),
		RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{
			LimitWindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
			// This mirrors the observed upstream inconsistency: reset_at says a
			// different window, while reset_after_seconds matches the live card.
			ResetAt:           fetchedAt.Add(7 * 24 * time.Hour).Unix(),
			ResetAfterSeconds: int64(wantReset.Sub(fetchedAt).Seconds()),
		}},
	}

	signal, err := findOpenAIWeeklyQuotaSignal(usage, fetchedAt.Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, wantReset, signal.resetAt)
	require.Equal(t, int64((7 * time.Hour * 24).Seconds()), signal.windowSeconds)
}

func TestFindOpenAIWeeklyQuotaObservationAllowsZeroUsageWithoutResetTime(t *testing.T) {
	usage := weeklyUsageWithPercent(0, nil)

	observation, err := findOpenAIWeeklyQuotaObservation(usage, time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Zero(t, observation.usedPercent)
	require.Nil(t, observation.resetAt)
	require.Equal(t, int64((7 * 24 * time.Hour).Seconds()), observation.windowSeconds)

	_, err = findOpenAIWeeklyQuotaSignal(usage, time.Now())
	require.ErrorIs(t, err, ErrUserWeeklyQuotaSyncNoWeeklyLimit)
}

func TestUserWeeklyQuotaSyncUsageDropResetsImmediatelyAndLaterCalibratesStart(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	previousReset := now.Add(2 * 24 * time.Hour)
	firstLowSeen := previousReset.Add(time.Minute)
	officialStart := firstLowSeen.Add(-2 * time.Minute)
	newReset := officialStart.Add(7 * 24 * time.Hour)
	resetter := &weeklyQuotaSyncResetter{ids: []int64{11, 12}}
	aligner := &weeklyQuotaSyncWindowAligner{ids: []int64{11, 12}}
	cache := &weeklyQuotaSyncCacheResetter{}
	service := newUserWeeklyQuotaSyncServiceForTest(t, &now, []*OpenAIQuotaUsage{
		weeklyUsageWithPercent(65, &previousReset),
		weeklyUsageWithPercent(0, nil),
		weeklyUsageWithPercent(3, &newReset),
	}, resetter, aligner, cache, nil)

	baseline, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, baseline.BaselineInitialized)

	now = firstLowSeen
	reset, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, reset.ResetDetected)
	require.False(t, reset.AwaitingConfirmation)
	require.Equal(t, userWeeklyQuotaSyncSignalUsagePercentDrop, reset.DetectionSignal)
	require.Equal(t, []time.Time{firstLowSeen}, resetter.starts)
	require.Equal(t, 1, cache.calls)
	require.Equal(t, []int64{11, 12}, cache.ids)
	require.NotNil(t, reset.Status.State.PendingWindowStart)
	require.Equal(t, firstLowSeen, *reset.Status.State.PendingWindowStart)

	now = now.Add(time.Minute)
	aligned, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.False(t, aligned.ResetDetected)
	require.True(t, aligned.WindowStartAligned)
	require.Equal(t, 2, aligned.AffectedUsers)
	require.Len(t, resetter.starts, 1)
	require.Equal(t, []weeklyQuotaSyncWindowAlignCall{{expectedStart: firstLowSeen, newStart: officialStart}}, aligner.calls)
	require.Equal(t, 1, cache.alignCalls)
	require.Equal(t, []int64{11, 12}, cache.alignIDs)
	require.Equal(t, firstLowSeen, cache.alignExpectedStart)
	require.Equal(t, officialStart, cache.alignNewStart)
	require.Nil(t, aligned.Status.State.PendingWindowStart)
	require.NotNil(t, aligned.Status.State.PeakWeeklyUsedPercent)
	require.InDelta(t, 3, *aligned.Status.State.PeakWeeklyUsedPercent, 1e-9)
}

func TestUserWeeklyQuotaSyncUsageDropResetsOnlyOnceWhenUsageStaysZero(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	previousReset := now.Add(2 * 24 * time.Hour)
	firstLowSeen := previousReset.Add(time.Minute)
	resetter := &weeklyQuotaSyncResetter{ids: []int64{101}}
	service := newUserWeeklyQuotaSyncServiceForTest(t, &now, []*OpenAIQuotaUsage{
		weeklyUsageWithPercent(65, &previousReset),
		weeklyUsageWithPercent(0, nil),
		weeklyUsageWithPercent(0, nil),
	}, resetter, nil, nil, nil)

	_, err := service.CheckNow(ctx)
	require.NoError(t, err)
	now = firstLowSeen
	first, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, first.ResetDetected)
	now = now.Add(time.Minute)
	second, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.False(t, second.ResetDetected)
	require.Equal(t, []time.Time{firstLowSeen}, resetter.starts)
}

func TestUserWeeklyQuotaSyncUsageDropAtThreePercentDoesNotRepeatAtSixPercent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	previousReset := now.Add(2 * 24 * time.Hour)
	firstLowSeen := previousReset.Add(time.Minute)
	officialStart := firstLowSeen.Add(-time.Minute)
	newReset := officialStart.Add(7 * 24 * time.Hour)
	resetter := &weeklyQuotaSyncResetter{ids: []int64{101}}
	aligner := &weeklyQuotaSyncWindowAligner{ids: []int64{101}}
	service := newUserWeeklyQuotaSyncServiceForTest(t, &now, []*OpenAIQuotaUsage{
		weeklyUsageWithPercent(65, &previousReset),
		weeklyUsageWithPercent(3, nil),
		weeklyUsageWithPercent(6, &newReset),
	}, resetter, aligner, nil, nil)

	_, err := service.CheckNow(ctx)
	require.NoError(t, err)
	now = firstLowSeen
	first, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, first.ResetDetected)
	now = now.Add(time.Minute)
	second, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.False(t, second.ResetDetected)
	require.True(t, second.WindowStartAligned)
	require.Equal(t, []time.Time{firstLowSeen}, resetter.starts)
	require.Len(t, aligner.calls, 1)
	require.NotNil(t, second.Status.State.PeakWeeklyUsedPercent)
	require.InDelta(t, 6, *second.Status.State.PeakWeeklyUsedPercent, 1e-9)
}

func TestUserWeeklyQuotaSyncEightPercentToZeroResetsImmediately(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	previousReset := now.Add(24 * time.Hour)
	resetAt := previousReset.Add(time.Minute)
	resetter := &weeklyQuotaSyncResetter{ids: []int64{101}}
	service := newUserWeeklyQuotaSyncServiceForTest(t, &now, []*OpenAIQuotaUsage{
		weeklyUsageWithPercent(8, &previousReset),
		weeklyUsageWithPercent(0, nil),
	}, resetter, nil, nil, nil)

	_, err := service.CheckNow(ctx)
	require.NoError(t, err)
	now = resetAt
	result, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, result.ResetDetected)
	require.Equal(t, []time.Time{resetAt}, resetter.starts)
}

func TestUserWeeklyQuotaSyncResetTimeChangeAloneDoesNotResetUsers(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	previousReset := now.Add(7 * 24 * time.Hour)
	changedReset := previousReset.Add(7 * 24 * time.Hour)
	resetter := &weeklyQuotaSyncResetter{ids: []int64{101}}
	service := newUserWeeklyQuotaSyncServiceForTest(t, &now, []*OpenAIQuotaUsage{
		weeklyUsageWithPercent(65, &previousReset),
		weeklyUsageWithPercent(65, &changedReset),
	}, resetter, nil, nil, nil)

	_, err := service.CheckNow(ctx)
	require.NoError(t, err)
	now = previousReset.Add(time.Minute)
	result, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.False(t, result.ResetDetected)
	require.Empty(t, resetter.starts)
	require.NotNil(t, result.Status.State.ObservedResetAt)
	require.Equal(t, changedReset, *result.Status.State.ObservedResetAt)
}

func TestUserWeeklyQuotaSyncExhaustedSourceIsRecoveredAfterReset(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	previousReset := now.Add(2 * 24 * time.Hour)
	resetAt := previousReset.Add(time.Minute)
	resetter := &weeklyQuotaSyncResetter{ids: []int64{101, 102}}
	clearer := &weeklyQuotaSyncRateLimitClearer{}
	service := newUserWeeklyQuotaSyncServiceForTest(t, &now, []*OpenAIQuotaUsage{
		weeklyUsageWithPercent(100, &previousReset),
		weeklyUsageWithPercent(3, nil),
	}, resetter, nil, nil, clearer)

	_, err := service.CheckNow(ctx)
	require.NoError(t, err)
	now = resetAt
	result, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, result.ResetDetected)
	require.True(t, result.SourceAccountRecovered)
	require.Equal(t, []int64{9}, clearer.calls)
	require.NotNil(t, result.Status.State.LastSourceAccountRecoveredAt)
	require.Equal(t, resetAt, *result.Status.State.LastSourceAccountRecoveredAt)
}

func TestUserWeeklyQuotaSyncRetriesFailedSourceRecoveryWithoutResettingUsersAgain(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	previousReset := now.Add(2 * 24 * time.Hour)
	resetAt := previousReset.Add(time.Minute)
	resetter := &weeklyQuotaSyncResetter{ids: []int64{101, 102}}
	clearer := &weeklyQuotaSyncRateLimitClearer{errs: []error{errors.New("temporary recovery failure")}}
	service := newUserWeeklyQuotaSyncServiceForTest(t, &now, []*OpenAIQuotaUsage{
		weeklyUsageWithPercent(100, &previousReset),
		weeklyUsageWithPercent(3, nil),
		weeklyUsageWithPercent(4, nil),
	}, resetter, nil, nil, clearer)

	_, err := service.CheckNow(ctx)
	require.NoError(t, err)
	now = resetAt
	first, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, first.ResetDetected)
	require.False(t, first.SourceAccountRecovered)
	require.True(t, first.Status.State.PendingSourceAccountRecovery)
	require.Len(t, resetter.starts, 1)

	now = now.Add(time.Minute)
	retry, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.False(t, retry.ResetDetected)
	require.True(t, retry.SourceAccountRecovered)
	require.False(t, retry.Status.State.PendingSourceAccountRecovery)
	require.Len(t, resetter.starts, 1)
	require.Equal(t, []int64{9, 9}, clearer.calls)
}

func TestUserWeeklyQuotaSyncDoesNotRecoverExhaustedSourceAboveFivePercent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	previousReset := now.Add(2 * 24 * time.Hour)
	resetAt := previousReset.Add(time.Minute)
	resetter := &weeklyQuotaSyncResetter{ids: []int64{101}}
	clearer := &weeklyQuotaSyncRateLimitClearer{}
	service := newUserWeeklyQuotaSyncServiceForTest(t, &now, []*OpenAIQuotaUsage{
		weeklyUsageWithPercent(100, &previousReset),
		weeklyUsageWithPercent(6, nil),
	}, resetter, nil, nil, clearer)

	_, err := service.CheckNow(ctx)
	require.NoError(t, err)
	now = resetAt
	result, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, result.ResetDetected)
	require.False(t, result.SourceAccountRecovered)
	require.Empty(t, clearer.calls)
}

func TestUserWeeklyQuotaSyncMigratesLegacyObservationWithoutReset(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 18, 12, 59, 23, 0, time.UTC)
	legacyObserved := now.Add(7 * 24 * time.Hour)
	matchedCardReset := now.Add(6*24*time.Hour + 14*time.Hour + 25*time.Minute + 38*time.Second)
	config := UserWeeklyQuotaSyncConfig{Enabled: true, SourceAccountID: 9, PollIntervalSeconds: 60}
	configJSON, err := json.Marshal(config)
	require.NoError(t, err)
	legacyStateJSON, err := json.Marshal(UserWeeklyQuotaSyncState{
		SourceAccountID:       9,
		ObservedResetAt:       &legacyObserved,
		ObservedWindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
		PendingResetAt:        &legacyObserved,
	})
	require.NoError(t, err)
	settings := &weeklyQuotaSyncSettingRepo{values: map[string]string{
		SettingKeyUserWeeklyQuotaSyncConfig: string(configJSON),
		SettingKeyUserWeeklyQuotaSyncState:  string(legacyStateJSON),
	}}
	accounts := &weeklyQuotaSyncAccountRepo{accounts: map[int64]*Account{
		9: {ID: 9, Name: "Codex Pro", Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive},
	}}
	usage := &OpenAIQuotaUsage{
		FetchedAt: now.Unix(),
		RateLimit: &OpenAIRateLimit{PrimaryWindow: &OpenAIRateLimitWindow{
			LimitWindowSeconds: int64((7 * 24 * time.Hour).Seconds()),
			ResetAt:            legacyObserved.Unix(),
			ResetAfterSeconds:  int64(matchedCardReset.Sub(now).Seconds()),
		}},
	}
	resetter := &weeklyQuotaSyncResetter{ids: []int64{101}}
	service := &UserWeeklyQuotaSyncService{
		settingRepo:  settings,
		accountRepo:  accounts,
		quotaService: &weeklyQuotaSyncUsageReader{responses: []*OpenAIQuotaUsage{usage}},
		bulkResetter: resetter,
		instanceID:   "test-instance",
		now:          func() time.Time { return now },
	}

	result, err := service.CheckNow(ctx)
	require.NoError(t, err)
	require.True(t, result.BaselineInitialized)
	require.False(t, result.ResetDetected)
	require.Empty(t, resetter.starts)
	require.NotNil(t, result.Status.State.ObservedResetAt)
	require.Equal(t, matchedCardReset, *result.Status.State.ObservedResetAt)
	require.Equal(t, userWeeklyQuotaSyncObservationSource, result.Status.State.ObservedSource)
	require.Nil(t, result.Status.State.PendingResetAt)
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
