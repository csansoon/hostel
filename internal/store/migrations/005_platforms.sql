-- M9: platforms in the database. The binary has the default platforms; a
-- revision replaces a platform with its files, removes it, or sets it back to
-- the default. The latest revision of each platform applies.
CREATE TABLE platform_revisions (
	platform   TEXT    NOT NULL,
	revision   INTEGER NOT NULL,
	state      TEXT    NOT NULL, -- files, removed, or default
	files      TEXT    NOT NULL DEFAULT '{}', -- JSON: file name → content
	user_id    INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (platform, revision)
);
