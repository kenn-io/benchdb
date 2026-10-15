package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/db"
	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

func TestSyncAlertRulesMatchesOneOwnersRulesToTheList(t *testing.T) {
	_, store, _, ctx := newIngester(t)
	owner, err := store.GetOrCreateUserByEmail(ctx, "alerts@example.com", "Alerts", "!")
	require.NoError(t, err)
	other, err := store.GetOrCreateUserByEmail(ctx, "someone@example.com", "Someone", "!")
	require.NoError(t, err)
	_, err = store.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: other, Name: "app main", Repository: "https://github.com/example/app",
		Baseline: "parent", Threshold: 5, ThresholdZ: 2, Enabled: true, CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	commitReason := "commit"
	rules := []service.AlertRuleSpec{
		{Name: "app main", Repository: "git@github.com:example/app/", Baseline: "parent", RunReason: &commitReason},
		{Name: "lib main", Repository: "https://github.com/example/lib", Baseline: "parent", RunReason: &commitReason},
	}

	synced, err := syncRules(ctx, store, owner, rules, now)
	require.NoError(t, err)
	assert.Equal(t, service.AlertRulesSynced{Created: 2}, synced)
	byName := ownerRules(t, store, owner)
	assert.Equal(t, "https://github.com/example/app", byName["app main"].Repository, "the repository is normalized")
	assert.InDelta(t, 5.0, byName["lib main"].Threshold, 0, "defaults apply")

	synced, err = syncRules(ctx, store, owner, rules, now)
	require.NoError(t, err)
	assert.Equal(t, service.AlertRulesSynced{Unchanged: 2}, synced, "a rerun changes nothing")

	// An open rule stays open when only enabled changes, and resets when what
	// it evaluates changes.
	for _, name := range []string{"app main", "lib main"} {
		_, err = store.UpdateAlertRuleEvaluation(ctx, storage.UpdateAlertRuleEvaluationParams{
			ID: byName[name].ID, State: storage.AlertRuleStateOpen, EvaluatedAt: now,
		})
		require.NoError(t, err)
	}
	disabled, threshold := false, 3.0
	rules[0].Enabled = &disabled
	rules[1].ThresholdZ = &threshold
	synced, err = syncRules(ctx, store, owner, rules, now)
	require.NoError(t, err)
	assert.Equal(t, service.AlertRulesSynced{Updated: 2}, synced)
	byName = ownerRules(t, store, owner)
	assert.False(t, byName["app main"].Enabled)
	assert.Equal(t, storage.AlertRuleStateOpen, byName["app main"].State)
	assert.Equal(t, storage.AlertRuleStateInactive, byName["lib main"].State)

	synced, err = syncRules(ctx, store, owner, rules[1:], now)
	require.NoError(t, err)
	assert.Equal(t, service.AlertRulesSynced{Deleted: 1, Unchanged: 1}, synced, "a rule left out of the list is deleted")
	assert.Len(t, ownerRules(t, store, owner), 1)
	assert.Len(t, ownerRules(t, store, other), 1, "another owner's rules are untouched")
}

func TestSyncAlertRulesRejectsAnInvalidListBeforeChangingAnything(t *testing.T) {
	_, store, _, ctx := newIngester(t)
	owner, err := store.GetOrCreateUserByEmail(ctx, "alerts@example.com", "Alerts", "!")
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = syncRules(ctx, store, owner, []service.AlertRuleSpec{
		{Name: "keep", Repository: "https://github.com/example/lib"},
	}, now)
	require.NoError(t, err)

	for name, rules := range map[string][]service.AlertRuleSpec{
		"bad baseline": {{Name: "new", Repository: "https://github.com/example/cli", Baseline: "nightly"}},
		"duplicate name": {
			{Name: "new", Repository: "https://github.com/example/cli"},
			{Name: "new", Repository: "https://github.com/example/app"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := syncRules(ctx, store, owner, rules, now)
			var ve *service.ValidationError
			require.ErrorAs(t, err, &ve)
			byName := ownerRules(t, store, owner)
			assert.Contains(t, byName, "keep", "the existing rule is not deleted")
			assert.NotContains(t, byName, "new")
		})
	}
}

func ownerRules(t *testing.T, store service.AlertRuleSyncStore, owner string) map[string]storage.AlertRule {
	t.Helper()
	rules, err := store.ListAlertRulesByUser(t.Context(), owner)
	require.NoError(t, err)
	byName := map[string]storage.AlertRule{}
	for _, rule := range rules {
		byName[rule.Name] = rule
	}
	return byName
}

func TestSyncAlertRulesKeepsTheOldestOfSameNamedRules(t *testing.T) {
	_, store, _, ctx := newIngester(t)
	owner, err := store.GetOrCreateUserByEmail(ctx, "alerts@example.com", "Alerts", "!")
	require.NoError(t, err)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var oldest string
	for i := range 3 {
		rule, err := store.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
			UserID: owner, Name: "app main", Repository: "https://github.com/example/app",
			Baseline: "fork_point", Threshold: 5, ThresholdZ: 2, Enabled: true,
			CreatedAt: start.Add(time.Duration(2-i) * time.Hour),
		})
		require.NoError(t, err)
		oldest = rule.ID
	}
	specs := []service.AlertRuleSpec{{Name: "app main", Repository: "https://github.com/example/app"}}

	synced, err := syncRules(ctx, store, owner, specs, start)
	require.NoError(t, err)
	assert.Equal(t, service.AlertRulesSynced{Deleted: 2, Unchanged: 1}, synced)
	rules, err := store.ListAlertRulesByUser(ctx, owner)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, oldest, rules[0].ID)
}

func TestSyncAlertRulesForOneOwnerRunOneAtATime(t *testing.T) {
	_, store, pool, ctx := newIngester(t)
	owner, err := store.GetOrCreateUserByEmail(ctx, "alerts@example.com", "Alerts", "!")
	require.NoError(t, err)
	now := time.Now().UTC()
	specs := []service.AlertRuleSpec{
		{Name: "app main", Repository: "https://github.com/example/app"},
		{Name: "lib main", Repository: "https://github.com/example/lib"},
	}
	firstWrote, release, firstDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFirst)
	go func() {
		firstDone <- store.WithAlertRuleLock(ctx, owner, func(tx *db.Store) error {
			if _, err := service.SyncAlertRules(ctx, tx, owner, specs, now); err != nil {
				return err
			}
			close(firstWrote)
			<-release
			return nil
		})
	}()
	<-firstWrote
	type result struct {
		synced service.AlertRulesSynced
		err    error
	}
	secondDone := make(chan result, 1)
	go func() {
		synced, err := syncRules(ctx, store, owner, specs, now)
		secondDone <- result{synced, err}
	}()

	require.Eventually(t, func() bool {
		var waiting int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted
			  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting)
		return err == nil && waiting == 1
	}, 10*time.Second, 10*time.Millisecond, "the second sync waits for the first one's lock")
	releaseFirst()
	require.NoError(t, <-firstDone)
	second := <-secondDone
	require.NoError(t, second.err)
	assert.Equal(t, service.AlertRulesSynced{Unchanged: 2}, second.synced, "the second sync sees the first one's rules")
	rules, err := store.ListAlertRulesByUser(ctx, owner)
	require.NoError(t, err)
	assert.Len(t, rules, 2)
}

// failingCreate fails to create one named rule, after the sync has already
// deleted and created others.
type failingCreate struct {
	*db.Store
	name string
}

func (s failingCreate) CreateAlertRule(ctx context.Context, p storage.InsertAlertRuleParams) (storage.AlertRule, error) {
	if p.Name == s.name {
		return storage.AlertRule{}, errors.New("insert failed")
	}
	return s.Store.CreateAlertRule(ctx, p)
}

func TestSyncAlertRulesFailurePartWayChangesNothing(t *testing.T) {
	_, store, _, ctx := newIngester(t)
	owner, err := store.GetOrCreateUserByEmail(ctx, "alerts@example.com", "Alerts", "!")
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = syncRules(ctx, store, owner, []service.AlertRuleSpec{
		{Name: "old", Repository: "https://github.com/example/cli"},
	}, now)
	require.NoError(t, err)

	err = store.WithAlertRuleLock(ctx, owner, func(tx *db.Store) error {
		_, err := service.SyncAlertRules(ctx, failingCreate{tx, "lib main"}, owner, []service.AlertRuleSpec{
			{Name: "app main", Repository: "https://github.com/example/app"},
			{Name: "lib main", Repository: "https://github.com/example/lib"},
		}, now)
		return err
	})

	require.ErrorContains(t, err, "insert failed")
	byName := ownerRules(t, store, owner)
	assert.Contains(t, byName, "old", "the deletion is rolled back")
	assert.NotContains(t, byName, "app main", "the creation is rolled back")
}

func syncRules(
	ctx context.Context, store *db.Store, owner string, specs []service.AlertRuleSpec, now time.Time,
) (service.AlertRulesSynced, error) {
	var synced service.AlertRulesSynced
	err := store.WithAlertRuleLock(ctx, owner, func(tx *db.Store) error {
		var err error
		synced, err = service.SyncAlertRules(ctx, tx, owner, specs, now)
		return err
	})
	return synced, err
}
