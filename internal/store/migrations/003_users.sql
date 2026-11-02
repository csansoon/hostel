-- M8: users and invites. A user token (role "user") belongs to a user and a
-- computer; room tokens (role "room") have no user.
CREATE TABLE users (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT    NOT NULL,
	role       TEXT    NOT NULL, -- admin or member
	created_at INTEGER NOT NULL,
	removed_at INTEGER
);
CREATE UNIQUE INDEX users_name ON users (name COLLATE NOCASE) WHERE removed_at IS NULL;

ALTER TABLE tokens ADD COLUMN user_id INTEGER REFERENCES users (id);
ALTER TABLE tokens ADD COLUMN computer TEXT NOT NULL DEFAULT '';

CREATE TABLE invites (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	kind          TEXT    NOT NULL, -- user (a new user) or computer (a token for user_id)
	secret_sha256 TEXT    NOT NULL UNIQUE,
	role          TEXT    NOT NULL DEFAULT '',
	name          TEXT    NOT NULL DEFAULT '',
	user_id       INTEGER REFERENCES users (id),
	created_by    INTEGER REFERENCES users (id),
	created_at    INTEGER NOT NULL,
	expires_at    INTEGER NOT NULL,
	used_at       INTEGER
);

-- An admin's queue order: a lease with manual_position > 0 comes before the
-- others, in this order.
ALTER TABLE leases ADD COLUMN manual_position INTEGER NOT NULL DEFAULT 0;

-- The user who caused an event (an admin action); 0 for the server.
ALTER TABLE events ADD COLUMN user_id INTEGER NOT NULL DEFAULT 0;

-- Existing guest and admin tokens become users: one user for each name and
-- role of tokens that are not revoked. Names are equal when hostel_fold (the
-- Unicode case folding of strings.EqualFold, registered by the store) is equal.
-- A guest token never gets admin rights: when an admin token has the same name,
-- the guest tokens' user is "<name> (member)", or "<name> (member N)" with the
-- lowest N >= 2 when that name is taken. Two such names of different groups
-- cannot be equal: " (member" occurs once in each suffix.
INSERT INTO users (name, role, created_at)
	SELECT MIN(name), 'admin', MIN(created_at) FROM tokens
	WHERE role = 'admin' AND revoked_at IS NULL GROUP BY hostel_fold(name);
CREATE TEMP TABLE m8_guests AS
	SELECT hostel_fold(name) AS fold, MIN(name) AS base, MIN(created_at) AS created_at,
		EXISTS (SELECT 1 FROM users a WHERE hostel_fold(a.name) = hostel_fold(t.name)) AS clash
	FROM tokens t WHERE role = 'guest' AND revoked_at IS NULL GROUP BY hostel_fold(name);
CREATE TEMP TABLE m8_members AS
	WITH RECURSIVE k(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM k WHERE n <= (SELECT COUNT(*) FROM tokens)),
	cand AS (
		SELECT g.fold, k.n, g.base || CASE k.n WHEN 1 THEN ' (member)' ELSE ' (member ' || k.n || ')' END AS name
		FROM m8_guests g, k WHERE g.clash),
	free AS (
		SELECT * FROM cand c
		WHERE NOT EXISTS (SELECT 1 FROM users u WHERE hostel_fold(u.name) = hostel_fold(c.name))
			AND NOT EXISTS (SELECT 1 FROM m8_guests o WHERE o.fold = hostel_fold(c.name)))
	SELECT f.fold, f.name FROM free f WHERE f.n = (SELECT MIN(n) FROM free f2 WHERE f2.fold = f.fold)
	UNION ALL
	SELECT fold, base FROM m8_guests WHERE NOT clash;
INSERT INTO users (name, role, created_at)
	SELECT m.name, 'member', g.created_at FROM m8_members m JOIN m8_guests g ON g.fold = m.fold;
UPDATE tokens SET user_id = (SELECT id FROM users WHERE hostel_fold(users.name) = hostel_fold(tokens.name) AND users.role = 'admin')
	WHERE role = 'admin' AND revoked_at IS NULL;
UPDATE tokens SET user_id = (SELECT u.id FROM m8_members m JOIN users u ON u.name = m.name AND u.role = 'member'
		WHERE m.fold = hostel_fold(tokens.name))
	WHERE role = 'guest' AND revoked_at IS NULL;
UPDATE tokens SET role = 'user' WHERE role IN ('guest', 'admin');
DROP TABLE m8_members;
DROP TABLE m8_guests;
