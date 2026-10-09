-- Saved CI reports are snapshots. Reports saved before comparison baselines
-- carried their own unit lack baseline.unit, so copy each baseline result's
-- stored unit into the snapshot. Measurements and verdicts are unchanged. A
-- baseline whose result was deleted gets a null unit.
UPDATE ci_run_report AS saved
SET report = jsonb_set(saved.report, '{runs}', (
    SELECT coalesce(jsonb_agg(
        CASE
            WHEN jsonb_typeof(runs.run -> 'comparisons') = 'array' THEN jsonb_set(runs.run, '{comparisons}', (
                SELECT coalesce(jsonb_agg(
                    CASE
                        WHEN jsonb_typeof(rows.row -> 'baseline') = 'object'
                            AND NOT (rows.row -> 'baseline' ? 'unit')
                        THEN jsonb_set(rows.row, '{baseline,unit}', coalesce(to_jsonb(baseline.unit), 'null'::jsonb))
                        ELSE rows.row
                    END
                    ORDER BY rows.position
                ), '[]'::jsonb)
                FROM jsonb_array_elements(runs.run -> 'comparisons') WITH ORDINALITY AS rows(row, position)
                LEFT JOIN benchmark_result AS baseline ON baseline.id = rows.row -> 'baseline' ->> 'result_id'
            ))
            ELSE runs.run
        END
        ORDER BY runs.position
    ), '[]'::jsonb)
    FROM jsonb_array_elements(saved.report -> 'runs') WITH ORDINALITY AS runs(run, position)
))
WHERE jsonb_typeof(saved.report -> 'runs') = 'array';
