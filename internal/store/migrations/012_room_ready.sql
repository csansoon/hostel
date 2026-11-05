-- M10e: ready_at is the first time that a room was running; with created_at
-- it gives the creation time of a new room for the estimated wait.
ALTER TABLE rooms ADD COLUMN ready_at INTEGER;
