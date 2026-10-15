package service_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/benchdb/internal/service"
	"go.kenn.io/benchdb/internal/storage"
)

func TestApplyAlertRulesSyncsOneOwnersRules(t *testing.T) {
	_, store, _, ctx := newIngester(t)
	owner, err := store.GetOrCreateUserByEmail(ctx, "alerts@example.com", "Alerts", "!")
	require.NoError(t, err)
	other, err := store.GetOrCreateUserByEmail(ctx, "someone@example.com", "Someone", "!")
	require.NoError(t, err)
	_, err = store.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
		UserID: other, Name: "agentsview main", Repository: "https://github.com/kenn-io/agentsview",
		Baseline: "parent", Threshold: 5, ThresholdZ: 2, Enabled: true, CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	commitReason := "commit"
	rules := []service.AlertRuleSpec{
		{Name: "agentsview main", Repository: "git@github.com:kenn-io/agentsview/", Baseline: "parent", RunReason: &commitReason},
		{Name: "docbank main", Repository: "https://github.com/kenn-io/docbank", Baseline: "parent", RunReason: &commitReason},
	}

	applied, err := service.ApplyAlertRules(ctx, store, owner, rules, now)
	require.NoError(t, err)
	assert.Equal(t, service.AlertRulesApplied{Created: 2}, applied)
	byName := ownerRules(t, store, owner)
	assert.Equal(t, "https://github.com/kenn-io/agentsview", byName["agentsview main"].Repository, "the repository is normalized")
	assert.InDelta(t, 5.0, byName["docbank main"].Threshold, 0, "defaults apply")

	applied, err = service.ApplyAlertRules(ctx, store, owner, rules, now)
	require.NoError(t, err)
	assert.Equal(t, service.AlertRulesApplied{Unchanged: 2}, applied, "a rerun changes nothing")

	// An open rule stays open when only enabled changes, and resets when what
	// it evaluates changes.
	for _, name := range []string{"agentsview main", "docbank main"} {
		_, err = store.UpdateAlertRuleEvaluation(ctx, storage.UpdateAlertRuleEvaluationParams{
			ID: byName[name].ID, State: storage.AlertRuleStateOpen, EvaluatedAt: now,
		})
		require.NoError(t, err)
	}
	disabled, threshold := false, 3.0
	rules[0].Enabled = &disabled
	rules[1].ThresholdZ = &threshold
	applied, err = service.ApplyAlertRules(ctx, store, owner, rules, now)
	require.NoError(t, err)
	assert.Equal(t, service.AlertRulesApplied{Updated: 2}, applied)
	byName = ownerRules(t, store, owner)
	assert.False(t, byName["agentsview main"].Enabled)
	assert.Equal(t, storage.AlertRuleStateOpen, byName["agentsview main"].State)
	assert.Equal(t, storage.AlertRuleStateInactive, byName["docbank main"].State)

	applied, err = service.ApplyAlertRules(ctx, store, owner, rules[1:], now)
	require.NoError(t, err)
	assert.Equal(t, service.AlertRulesApplied{Deleted: 1, Unchanged: 1}, applied, "a rule left out of the list is deleted")
	assert.Len(t, ownerRules(t, store, owner), 1)
	assert.Len(t, ownerRules(t, store, other), 1, "another owner's rules are untouched")
}

func TestApplyAlertRulesRejectsAnInvalidListBeforeChangingAnything(t *testing.T) {
	_, store, _, ctx := newIngester(t)
	owner, err := store.GetOrCreateUserByEmail(ctx, "alerts@example.com", "Alerts", "!")
	require.NoError(t, err)
	now := time.Now().UTC()
	_, err = service.ApplyAlertRules(ctx, store, owner, []service.AlertRuleSpec{
		{Name: "keep", Repository: "https://github.com/kenn-io/docbank"},
	}, now)
	require.NoError(t, err)

	for name, rules := range map[string][]service.AlertRuleSpec{
		"bad baseline": {{Name: "new", Repository: "https://github.com/kenn-io/msgvault", Baseline: "nightly"}},
		"duplicate name": {
			{Name: "new", Repository: "https://github.com/kenn-io/msgvault"},
			{Name: "new", Repository: "https://github.com/kenn-io/agentsview"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := service.ApplyAlertRules(ctx, store, owner, rules, now)
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
