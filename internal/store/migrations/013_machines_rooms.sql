-- M10g: a machine is a real computer or VM, a room is a place on it for one
-- lease, and a template is what an on-demand provider creates machines from.
ALTER TABLE rooms RENAME TO machines;
ALTER TABLE slots RENAME TO rooms;
ALTER TABLE room_tools RENAME TO machine_tools;
ALTER TABLE machines RENAME COLUMN machine TO template;
ALTER TABLE rooms RENAME COLUMN room_id TO machine_id;
ALTER TABLE machine_tools RENAME COLUMN room_id TO machine_id;
ALTER TABLE leases RENAME COLUMN machine TO template;
ALTER TABLE leases RENAME COLUMN room_id TO machine_id;
ALTER TABLE leases RENAME COLUMN slot_name TO room_name;
ALTER TABLE events RENAME COLUMN room_id TO machine_id;
ALTER TABLE invites RENAME COLUMN machine TO template;
DROP INDEX rooms_state;
CREATE INDEX machines_state ON machines (state);
DROP INDEX events_room;
CREATE INDEX events_machine ON events (machine_id);
ALTER TABLE tokens RENAME COLUMN machine TO template;
UPDATE tokens SET role = 'machine' WHERE role = 'room';
ALTER TABLE lease_tunnels RENAME COLUMN room_port TO machine_port;
UPDATE leases SET tunnels = replace(tunnels, '"room_port"', '"machine_port"');
-- Own tools keep working: the room's and the machine's names in their scripts.
UPDATE tool_revisions SET files = replace(replace(files, 'HOSTEL_ROOM', 'HOSTEL_MACHINE'), 'HOSTEL_SLOT', 'HOSTEL_ROOM');
-- The directories of the rooms move on the machines, so open leases end. One
-- with a room goes to cleaning, which frees its room; the others fail at once.
UPDATE leases SET state = 'cleaning', end_state = 'failed', end_reason = 'the server was updated; acquire again',
	revision = revision + 1
	WHERE state NOT IN ('released', 'expired', 'cancelled', 'failed', 'cleaning') AND machine_id IS NOT NULL;
UPDATE leases SET state = 'failed', end_state = 'failed', end_reason = 'the server was updated; acquire again',
	ended_at = CAST(strftime('%s', 'now') AS INTEGER) * 1000, revision = revision + 1
	WHERE state NOT IN ('released', 'expired', 'cancelled', 'failed', 'cleaning');
