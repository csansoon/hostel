-- M10e: a lease asks for tools (owner decision 2026-11-05: no compatibility
-- with the leases of platforms). Open leases end: one with a slot goes to
-- cleaning, which frees its slot; the others fail at once.
UPDATE leases SET state = 'cleaning', end_state = 'failed', end_reason = 'the server was updated to tools; acquire again',
	revision = revision + 1
	WHERE state NOT IN ('released', 'expired', 'cancelled', 'failed', 'cleaning') AND room_id IS NOT NULL;
UPDATE leases SET state = 'failed', end_state = 'failed', end_reason = 'the server was updated to tools; acquire again',
	ended_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000, revision = revision + 1
	WHERE state NOT IN ('released', 'expired', 'cancelled', 'failed', 'cleaning');

ALTER TABLE leases DROP COLUMN platform;
ALTER TABLE leases DROP COLUMN devices;
ALTER TABLE leases DROP COLUMN required_labels;
-- tools: the request, as "name" or "name:option" entries. started: the screen
-- tools that started for the lease, with their options and the names of their
-- facts (JSON: [{"name", "options", "facts"}]).
ALTER TABLE leases ADD COLUMN tools TEXT NOT NULL DEFAULT '[]';
ALTER TABLE leases ADD COLUMN started TEXT NOT NULL DEFAULT '[]';
