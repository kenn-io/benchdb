DROP TRIGGER IF EXISTS commit_verdicts ON commit;
DROP TRIGGER IF EXISTS benchmark_result_annotation_verdicts ON benchmark_result;
DROP TRIGGER IF EXISTS benchmark_result_verdicts ON benchmark_result;
DROP FUNCTION IF EXISTS queue_commit_verdicts();
DROP FUNCTION IF EXISTS queue_result_verdicts();
DROP FUNCTION IF EXISTS enqueue_verdict(text, text);
DROP TABLE IF EXISTS verdict_queue;
DROP TABLE IF EXISTS run_verdict;
