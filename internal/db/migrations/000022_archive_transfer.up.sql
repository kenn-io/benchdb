-- A stopped-deployment transfer copies the active archive to another storage
-- binding. Verified destination receipts survive interruption, so a resumed
-- transfer copies only the objects it has not verified. Cutover sets
-- cutover_at in the transaction that switches the active binding, so a lost
-- commit response is resolved from this row. activation_token is committed
-- before the destination's activation marker is published, ahead of cutover;
-- only a transfer holding the marker's token may resume into it.
CREATE TABLE archive_transfer (
    id uuid PRIMARY KEY,
    source_storage_id uuid NOT NULL REFERENCES archive_storage(id),
    destination_storage_id uuid NOT NULL REFERENCES archive_storage(id),
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    cutover_at timestamptz,
    activation_token uuid,
    CHECK (source_storage_id <> destination_storage_id)
);
-- One unfinished transfer at a time.
CREATE UNIQUE INDEX archive_transfer_unfinished ON archive_transfer((true)) WHERE cutover_at IS NULL;
CREATE TABLE archive_transfer_object (
    transfer_id uuid NOT NULL REFERENCES archive_transfer(id),
    key text NOT NULL,
    receipt jsonb NOT NULL,
    PRIMARY KEY(transfer_id,key)
);
