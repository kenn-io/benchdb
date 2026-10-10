UPDATE ci_run_report AS saved
SET report = jsonb_set(saved.report, '{runs}', (
    SELECT coalesce(jsonb_agg(
        CASE
            WHEN jsonb_typeof(runs.run -> 'comparisons') = 'array' THEN jsonb_set(runs.run, '{comparisons}', (
                SELECT coalesce(jsonb_agg(
                    CASE
                        WHEN jsonb_typeof(rows.row -> 'baseline') = 'object'
                        THEN jsonb_set(rows.row, '{baseline}', (rows.row -> 'baseline') - 'unit')
                        ELSE rows.row
                    END
                    ORDER BY rows.position
                ), '[]'::jsonb)
                FROM jsonb_array_elements(runs.run -> 'comparisons') WITH ORDINALITY AS rows(row, position)
            ))
            ELSE runs.run
        END
        ORDER BY runs.position
    ), '[]'::jsonb)
    FROM jsonb_array_elements(saved.report -> 'runs') WITH ORDINALITY AS runs(run, position)
))
WHERE jsonb_typeof(saved.report -> 'runs') = 'array';
