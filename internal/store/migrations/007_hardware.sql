-- M10: the hardware of a room (protocol.Hardware as JSON), from the helper
-- after each provisioning; empty: not known.
ALTER TABLE rooms ADD COLUMN hardware TEXT NOT NULL DEFAULT '';
