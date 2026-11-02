-- M9: machine invites (kind machine): "hostel machine join <code>" adds the
-- machine to the configuration. machine is JSON: provider, name, platforms,
-- labels.
ALTER TABLE invites ADD COLUMN machine TEXT NOT NULL DEFAULT '';
