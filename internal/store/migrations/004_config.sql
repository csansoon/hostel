-- M9: the configuration document (providers, machines, settings) in the
-- database. The latest revision applies; revision 1 is the import of
-- server.toml or the first change.
CREATE TABLE config_revisions (
	revision   INTEGER PRIMARY KEY,
	document   TEXT    NOT NULL,
	user_id    INTEGER NOT NULL DEFAULT 0, -- 0: the server (the import)
	created_at INTEGER NOT NULL
);

-- The hash of the machine settings that a room was provisioned or created
-- with; a free room with another hash provisions again (static) or is
-- destroyed (on demand). Empty: not known yet.
ALTER TABLE rooms ADD COLUMN config_sha256 TEXT NOT NULL DEFAULT '';
