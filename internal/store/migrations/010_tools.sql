-- M10e: tools replace platforms (owner decision 2026-11-05: no migration of
-- the platform revisions or of the rooms' platform state). The binary has the
-- default tools; a revision replaces a tool with its files, removes it, or
-- sets it back to the default. The latest revision of each tool applies.
DROP TABLE platform_revisions;
DROP TABLE room_platforms;

CREATE TABLE tool_revisions (
	tool       TEXT    NOT NULL,
	revision   INTEGER NOT NULL,
	state      TEXT    NOT NULL, -- files, removed, or default
	files      TEXT    NOT NULL DEFAULT '{}', -- JSON: file name → content
	user_id    INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (tool, revision)
);

-- The inventory of each room: each defined tool of its machine, the hash of
-- the tool files that installed it, its state, and the option that its check
-- script reported (a version, or the options of a screen tool).
CREATE TABLE room_tools (
	room_id        INTEGER NOT NULL REFERENCES rooms (id),
	tool           TEXT    NOT NULL,
	scripts_sha256 TEXT    NOT NULL,
	state          TEXT    NOT NULL, -- installing, ready, or failed
	option         TEXT    NOT NULL DEFAULT '',
	env            TEXT    NOT NULL DEFAULT '[]', -- JSON: the tool's environment names
	checked_at     INTEGER NOT NULL,
	PRIMARY KEY (room_id, tool)
);
