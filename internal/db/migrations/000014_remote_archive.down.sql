-- An old binary cannot preserve remote storage or retention promises. Restore
-- a matched database/archive backup instead of dropping those commitments.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM archive_storage WHERE backend <> 'file')
       OR EXISTS (SELECT 1 FROM archive_retirement)
       OR EXISTS (SELECT 1 FROM archive_orphan)
       OR EXISTS (SELECT 1 FROM archive_upload)
       OR EXISTS (SELECT 1 FROM archive_cycle)
       OR EXISTS (SELECT 1 FROM archive_file_location)
       OR EXISTS (SELECT 1 FROM archive_lookup_partition)
       OR EXISTS (SELECT 1 FROM archive_policy WHERE configured_retention_ns IS NOT NULL OR applied_retention_ns IS NOT NULL)
       OR EXISTS (SELECT 1 FROM archive_maintenance)
       OR EXISTS (SELECT 1 FROM archive_object WHERE verification_method IS NOT NULL)
    THEN
        RAISE EXCEPTION 'cannot downgrade archive lifecycle state; restore a matched database and archive backup';
    END IF;
END;
$$;
DROP TABLE archive_lookup_segment, archive_lookup_partition;
DROP TYPE archive_key_range;
DROP INDEX archive_file_commit_commit_id;
DROP TABLE archive_file_location, archive_maintenance, archive_policy, archive_orphan,
    archive_retirement, archive_upload, archive_cycle, archive_object, archive_storage;
DROP FUNCTION check_active_archive_storage();
