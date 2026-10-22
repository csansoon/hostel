-- Times are Unix milliseconds (UTC). NULL means "not set".

CREATE TABLE rooms (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	name              TEXT    NOT NULL UNIQUE,
	provider          TEXT    NOT NULL,
	machine           TEXT    NOT NULL,
	provider_ref      TEXT    NOT NULL DEFAULT '',
	state             TEXT    NOT NULL,
	os                TEXT    NOT NULL DEFAULT '',
	arch              TEXT    NOT NULL DEFAULT '',
	home              TEXT    NOT NULL DEFAULT '',
	ssh_host_key      TEXT    NOT NULL DEFAULT '',
	helper_sha256     TEXT    NOT NULL DEFAULT '',
	provider_deadline INTEGER,
	warm_until        INTEGER,
	drained           INTEGER NOT NULL DEFAULT 0,
	broken_reason     TEXT    NOT NULL DEFAULT '',
	created_at        INTEGER NOT NULL,
	state_changed_at  INTEGER NOT NULL
);
CREATE INDEX rooms_state ON rooms (state);

CREATE TABLE room_platforms (
	room_id        INTEGER NOT NULL REFERENCES rooms (id),
	platform       TEXT    NOT NULL,
	scripts_sha256 TEXT    NOT NULL,
	provisioned_at INTEGER NOT NULL,
	PRIMARY KEY (room_id, platform)
);

CREATE TABLE slots (
	room_id      INTEGER NOT NULL REFERENCES rooms (id),
	name         TEXT    NOT NULL,
	state        TEXT    NOT NULL,
	lease_id     TEXT    NOT NULL DEFAULT '',
	cleanup_data TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (room_id, name)
);

CREATE TABLE leases (
	id               TEXT    PRIMARY KEY,
	token_id         INTEGER NOT NULL REFERENCES tokens (id),
	state            TEXT    NOT NULL,
	revision         INTEGER NOT NULL DEFAULT 1,
	position         INTEGER NOT NULL DEFAULT 0,
	reason           TEXT    NOT NULL DEFAULT '',
	platform         TEXT    NOT NULL,
	devices          TEXT    NOT NULL DEFAULT '[]', -- JSON array
	task             TEXT    NOT NULL DEFAULT '',
	priority         INTEGER NOT NULL,
	required_labels  TEXT    NOT NULL DEFAULT '[]', -- JSON array
	tunnels          TEXT    NOT NULL DEFAULT '[]', -- JSON array of {"name", "room_port"} from the request
	fresh            INTEGER NOT NULL DEFAULT 0,
	public_key       TEXT    NOT NULL,
	guest_host       TEXT    NOT NULL DEFAULT '',
	repo             TEXT    NOT NULL DEFAULT '',
	worktree         TEXT    NOT NULL DEFAULT '',
	provider         TEXT    NOT NULL DEFAULT '',
	machine          TEXT    NOT NULL DEFAULT '',
	room_id          INTEGER REFERENCES rooms (id),
	slot_name        TEXT    NOT NULL DEFAULT '',
	attempts         INTEGER NOT NULL DEFAULT 0,
	began_at         INTEGER,
	created_at       INTEGER NOT NULL,
	last_poll_at     INTEGER NOT NULL,
	last_activity_at INTEGER NOT NULL,
	ready_at         INTEGER,
	expires_at       INTEGER,
	ended_at         INTEGER,
	-- end_state is the final state that a lease in cleaning goes to.
	end_state        TEXT    NOT NULL DEFAULT '',
	end_reason       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX leases_state ON leases (state);

CREATE TABLE lease_tunnels (
	lease_id  TEXT    NOT NULL REFERENCES leases (id),
	name      TEXT    NOT NULL,
	room_port INTEGER NOT NULL,
	PRIMARY KEY (lease_id, name)
);

CREATE TABLE tokens (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT    NOT NULL,
	role       TEXT    NOT NULL,
	machine    TEXT    NOT NULL DEFAULT '',
	sha256     TEXT    NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	revoked_at INTEGER
);

CREATE TABLE events (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	time     INTEGER NOT NULL,
	lease_id TEXT    NOT NULL DEFAULT '',
	room_id  INTEGER NOT NULL DEFAULT 0,
	kind     TEXT    NOT NULL,
	message  TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX events_lease ON events (lease_id);
CREATE INDEX events_room ON events (room_id);
