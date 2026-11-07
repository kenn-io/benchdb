package service

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"math"

	"go.kenn.io/benchdb/internal/storage"
)

// UpdateChangeAnnotations ports BenchmarkResult.update (benchmark_result.py:308):
// merge the given keys over the stored ones (given wins), then drop every
// null-valued key — that is how a key is deleted. Only change_annotations is
// updatable; everything else on the result is immutable.
func (i *Ingester) UpdateChangeAnnotations(ctx context.Context, id string, given map[string]any) error {
	if err := validateChangeAnnotations(given); err != nil {
		return err
	}
	row, err := i.store.GetBenchmarkResultByID(ctx, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("load result for update: %w", err)
	}
	return i.mergeChangeAnnotations(ctx, row, given)
}

// mergeChangeAnnotations merges given over the loaded result's
// annotations without validating them; callers validate first.
func (i *Ingester) mergeChangeAnnotations(ctx context.Context, row storage.BenchmarkResult, given map[string]any) error {
	id := row.ID
	old, err := jsonObject(row.ChangeAnnotations)
	if err != nil {
		return err
	}
	merged := make(map[string]any, len(old)+len(given))
	maps.Copy(merged, old)
	maps.Copy(merged, given)

	caJSON, err := json.Marshal(filterNullValues(merged))
	if err != nil {
		return fmt.Errorf("marshal change_annotations: %w", err)
	}
	if err := i.store.UpdateBenchmarkResultChangeAnnotations(ctx, id, caJSON); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// Concurrent delete between the load above and this update: still a 404.
			return ErrNotFound
		}
		return fmt.Errorf("update change_annotations: %w", err)
	}
	return nil
}

func validateChangeAnnotations(annotations map[string]any) error {
	if value := annotations["exclude_from_history"]; value != nil {
		if _, ok := value.(bool); !ok {
			return &ValidationError{Message: "change_annotations.exclude_from_history must be a boolean or null"}
		}
	}
	if value, ok := annotations[baselineOverrideKey]; ok && value != nil {
		return &ValidationError{Message: "change_annotations.baseline_override is computed from a selected range of results; it can only be removed (null) here"}
	}
	return nil
}

// SetBaselineOverride fixes the baseline of the result's series from that
// result onward. The value is the mean of the single value summaries of every
// series member committed from this result's commit through the commit of
// throughResultID, inclusive. It is stored on this result as the
// baseline_override change annotation, so measurements stay untouched and
// later reads, comparisons and alert checks score against it until another
// boundary begins. The range may not cross an existing distribution change or
// baseline override, which would otherwise split the selection.
func (i *Ingester) SetBaselineOverride(ctx context.Context, id, throughResultID string) error {
	result, err := i.store.GetBenchmarkResultByID(ctx, id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("load result for baseline override: %w", err)
	}
	rows, err := i.store.SelectHistoryForFingerprint(ctx, result.HistoryFingerprint)
	if err != nil {
		return fmt.Errorf("select history for baseline override: %w", err)
	}
	override, err := baselineOverrideFromHistory(rows, id, throughResultID)
	if err != nil {
		return err
	}
	return i.mergeChangeAnnotations(ctx, result, map[string]any{baselineOverrideKey: override})
}

// baselineOverrideFromHistory averages the members between the two results'
// commits, inclusive. Both results must be members of the series.
func baselineOverrideFromHistory(rows []storage.HistoryRow, startID, throughID string) (BaselineOverride, error) {
	var start, through *storage.HistoryRow
	for k := range rows {
		switch rows[k].ID {
		case startID:
			start = &rows[k]
		case throughID:
			through = &rows[k]
		}
	}
	if startID == throughID {
		through = start
	}
	if start == nil || through == nil || start.CommitTimestamp == nil || through.CommitTimestamp == nil {
		return BaselineOverride{}, &ValidationError{Message: "both results must be default-branch members of the same series"}
	}
	if through.CommitTimestamp.Before(*start.CommitTimestamp) {
		return BaselineOverride{}, &ValidationError{Message: "the last selected result must not be committed before the first"}
	}
	var sum float64
	count := 0
	for _, row := range rows {
		ct := row.CommitTimestamp
		if ct == nil || ct.Before(*start.CommitTimestamp) || ct.After(*through.CommitTimestamp) {
			continue
		}
		if !sameUnitPtr(row.Unit, start.Unit) {
			return BaselineOverride{}, &ValidationError{Message: "the selected results mix measurement units"}
		}
		annotations, err := decodeSegmentAnnotations(row.ChangeAnnotations)
		if err != nil {
			return BaselineOverride{}, err
		}
		if (ct.After(*start.CommitTimestamp) && (annotations.begins || annotations.override != nil)) ||
			(ct.Equal(*start.CommitTimestamp) && row.ID != startID && annotations.override != nil) {
			return BaselineOverride{}, &ValidationError{Message: "the selected range crosses a distribution change or another selected baseline; remove it or select a range that ends before it"}
		}
		svs, _, err := historySVS(row.Unit, row.Data)
		if err != nil {
			return BaselineOverride{}, err
		}
		sum += svs
		count++
	}
	value := sum / float64(count)
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return BaselineOverride{}, &ValidationError{Message: "the selected results have no finite average"}
	}
	return BaselineOverride{Value: value, ThroughResultID: through.ID, ResultCount: count}, nil
}

// DeleteResult hard-deletes one result row. Related case/context/info/
// hardware/commit rows are shared with other results and are never cascaded
// (the FK graph points outward from benchmark_result; nothing references it).
// History, series, and compare are derived per-query, so they recompute on
// the next read.
func (i *Ingester) DeleteResult(ctx context.Context, id string) error {
	if err := i.store.DeleteBenchmarkResult(ctx, id); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("delete result: %w", err)
	}
	return nil
}
