-- The shell of the room's SSH server: '' (sh), 'powershell', or 'cmd' (Windows).
ALTER TABLE rooms ADD COLUMN shell TEXT NOT NULL DEFAULT '';
-- The home directory of a slot's own OS user (static machines; empty: the
-- machine user's home, rooms.home).
ALTER TABLE slots ADD COLUMN home TEXT NOT NULL DEFAULT '';
