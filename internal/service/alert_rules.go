package service

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"go.kenn.io/benchdb/internal/commit"
	"go.kenn.io/benchdb/internal/stats"
	"go.kenn.io/benchdb/internal/storage"
)

// AlertRuleSpec is a requested alert rule. Omitted fields take their
// defaults: baseline fork_point, threshold 5, threshold_z 2, enabled.
type AlertRuleSpec struct {
	Name       string   `json:"name"`
	Repository string   `json:"repository"`
	Baseline   string   `json:"baseline,omitempty"`
	Threshold  *float64 `json:"threshold,omitempty"`
	ThresholdZ *float64 `json:"threshold_z,omitempty"`
	RunReason  *string  `json:"run_reason,omitempty"`
	Enabled    *bool    `json:"enabled,omitempty"`
}

// AlertRuleConfig is a validated alert rule with its defaults applied.
type AlertRuleConfig struct {
	Name       string
	Repository string
	Baseline   string
	Threshold  float64
	ThresholdZ float64
	RunReason  *string
	Enabled    bool
}

// NormalizeAlertRule validates a rule and applies its defaults. Invalid
// fields return a *ValidationError.
func NormalizeAlertRule(in AlertRuleSpec) (AlertRuleConfig, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return AlertRuleConfig{}, &ValidationError{Message: "name is required"}
	}
	repository := strings.TrimSpace(in.Repository)
	if repository == "" {
		return AlertRuleConfig{}, &ValidationError{Message: "repository is required"}
	}
	baseline := in.Baseline
	if baseline == "" {
		baseline = string(CIReportBaselineForkPoint)
	}
	if !validAlertBaseline(baseline) {
		return AlertRuleConfig{}, &ValidationError{Message: "invalid baseline"}
	}
	threshold, err := positiveOrDefault(in.Threshold, stats.PairwisePercentThresholdDefault, "threshold")
	if err != nil {
		return AlertRuleConfig{}, err
	}
	thresholdZ, err := positiveOrDefault(in.ThresholdZ, stats.ZScoreThresholdDefault, "threshold_z")
	if err != nil {
		return AlertRuleConfig{}, err
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	var runReason *string
	if in.RunReason != nil {
		if trimmed := strings.TrimSpace(*in.RunReason); trimmed != "" {
			runReason = &trimmed
		}
	}
	return AlertRuleConfig{
		Name: name, Repository: commit.NormalizeRepoURL(repository), Baseline: baseline,
		Threshold: threshold, ThresholdZ: thresholdZ, RunReason: runReason, Enabled: enabled,
	}, nil
}

func positiveOrDefault(value *float64, fallback float64, field string) (float64, error) {
	v := fallback
	if value != nil {
		v = *value
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, &ValidationError{Message: field + " must be greater than zero"}
	}
	return v, nil
}

func validAlertBaseline(v string) bool {
	return v == string(CIReportBaselineForkPoint) ||
		v == string(CIReportBaselineParent) ||
		v == string(CIReportBaselineLatestDefault)
}

// AlertRuleEvaluationChanged reports whether cfg changes what a rule
// evaluates, so its state must reset. A new name or enabled flag does not.
func AlertRuleEvaluationChanged(rule storage.AlertRule, cfg AlertRuleConfig) bool {
	return rule.Repository != cfg.Repository ||
		rule.Baseline != cfg.Baseline ||
		rule.Threshold != cfg.Threshold ||
		rule.ThresholdZ != cfg.ThresholdZ ||
		!sameStringPtr(rule.RunReason, cfg.RunReason)
}

// AlertRuleSyncStore is the storage ApplyAlertRules needs.
type AlertRuleSyncStore interface {
	ListAlertRulesByUser(ctx context.Context, userID string) ([]storage.AlertRule, error)
	CreateAlertRule(ctx context.Context, p storage.InsertAlertRuleParams) (storage.AlertRule, error)
	UpdateAlertRule(ctx context.Context, p storage.UpdateAlertRuleParams) (storage.AlertRule, error)
	DeleteAlertRule(ctx context.Context, id, userID string) error
}

// AlertRulesApplied counts what ApplyAlertRules changed.
type AlertRulesApplied struct {
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Deleted   int `json:"deleted"`
	Unchanged int `json:"unchanged"`
}

// ApplyAlertRules makes one owner's alert rules match specs exactly, matching
// rules by name. It creates missing rules, updates changed ones, and deletes
// the owner's rules that specs do not name, so the owner should be an account
// whose rules come only from this list. Every spec is validated, and names
// must be unique, before anything changes. A rerun after a partial failure
// finishes the job.
func ApplyAlertRules(
	ctx context.Context, store AlertRuleSyncStore, ownerID string, specs []AlertRuleSpec, now time.Time,
) (AlertRulesApplied, error) {
	var applied AlertRulesApplied
	wanted := make([]AlertRuleConfig, 0, len(specs))
	names := make(map[string]bool, len(specs))
	for i, spec := range specs {
		cfg, err := NormalizeAlertRule(spec)
		if err != nil {
			return applied, fmt.Errorf("rule %d: %w", i+1, err)
		}
		if names[cfg.Name] {
			return applied, &ValidationError{Message: fmt.Sprintf("rule %d: duplicate name %q", i+1, cfg.Name)}
		}
		names[cfg.Name] = true
		wanted = append(wanted, cfg)
	}
	existing, err := store.ListAlertRulesByUser(ctx, ownerID)
	if err != nil {
		return applied, fmt.Errorf("list alert rules: %w", err)
	}
	byName := make(map[string]storage.AlertRule, len(existing))
	for _, rule := range existing {
		if !names[rule.Name] {
			if err := store.DeleteAlertRule(ctx, rule.ID, ownerID); err != nil {
				return applied, fmt.Errorf("delete alert rule %q: %w", rule.Name, err)
			}
			applied.Deleted++
			continue
		}
		byName[rule.Name] = rule
	}
	for _, cfg := range wanted {
		rule, ok := byName[cfg.Name]
		if !ok {
			if _, err := store.CreateAlertRule(ctx, storage.InsertAlertRuleParams{
				UserID: ownerID, Name: cfg.Name, Repository: cfg.Repository, Baseline: cfg.Baseline,
				Threshold: cfg.Threshold, ThresholdZ: cfg.ThresholdZ, RunReason: cfg.RunReason,
				Enabled: cfg.Enabled, CreatedAt: now,
			}); err != nil {
				return applied, fmt.Errorf("create alert rule %q: %w", cfg.Name, err)
			}
			applied.Created++
			continue
		}
		reset := AlertRuleEvaluationChanged(rule, cfg)
		if !reset && rule.Enabled == cfg.Enabled {
			applied.Unchanged++
			continue
		}
		if _, err := store.UpdateAlertRule(ctx, storage.UpdateAlertRuleParams{
			ID: rule.ID, UserID: ownerID, Name: cfg.Name, Repository: cfg.Repository, Baseline: cfg.Baseline,
			Threshold: cfg.Threshold, ThresholdZ: cfg.ThresholdZ, RunReason: cfg.RunReason,
			Enabled: cfg.Enabled, UpdatedAt: now, ResetEvaluation: reset,
		}); err != nil {
			return applied, fmt.Errorf("update alert rule %q: %w", cfg.Name, err)
		}
		applied.Updated++
	}
	return applied, nil
}
