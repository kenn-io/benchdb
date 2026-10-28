-- An engine records the first time it claims an archive storage binding by
-- its dataset marker. Startup may move the active binding automatically only
-- away from the initial implicit local binding before any engine claimed it.
ALTER TABLE archive_storage ADD COLUMN claimed_at timestamptz;
