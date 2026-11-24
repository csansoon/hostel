-- M13: a remote update of a machine that connects: the release tag that it
-- gets ('' = none), and its drain state before the update.
ALTER TABLE machines ADD COLUMN update_to TEXT NOT NULL DEFAULT '';
ALTER TABLE machines ADD COLUMN update_drained INTEGER NOT NULL DEFAULT 0;
