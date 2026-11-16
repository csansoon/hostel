-- M11: the OS user that a lease got for its rooms ('' = none).
ALTER TABLE rooms ADD COLUMN lease_user TEXT NOT NULL DEFAULT '';
