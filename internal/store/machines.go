package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

type Machine struct {
	ID           int64
	Name         string
	Provider     string
	Template     string
	ProviderRef  string
	State        string
	OS           string
	Arch         string
	Home         string
	Shell        string
	SSHHostKey   string
	HelperSHA256 string
	ConfigSHA256 string
	Hardware     string
	// LastSeenAt is the last time that the machine answered a presence check.
	LastSeenAt       time.Time
	ProviderDeadline time.Time
	WarmUntil        time.Time
	Drained          bool
	BrokenReason     string
	CreatedAt        time.Time
	StateChangedAt   time.Time
}

type Room struct {
	MachineID   int64
	Name        string
	State       string
	LeaseID     string
	CleanupData string
	Home        string
	// LeaseUser is the OS user of the room's lease (M11), or "".
	LeaseUser string
}

// MachineTool is a row of the inventory of a machine (machine_tools).
type MachineTool struct {
	MachineID     int64
	Tool          string
	ScriptsSHA256 string
	State         string
	// Option is what the check script reported: a version, or the options
	// of a screen tool, comma-separated.
	Option    string
	Env       []string
	CheckedAt time.Time
}

// Machine tool states.
const (
	ToolInstalling = "installing"
	ToolReady      = "ready"
	ToolFailed     = "failed"
)

const machineColumns = `id, name, provider, template, provider_ref, state, os, arch, home, shell, ssh_host_key,
	helper_sha256, config_sha256, hardware, provider_deadline, warm_until, drained, broken_reason, created_at, state_changed_at, last_seen_at`

func scanMachine(row interface{ Scan(...any) error }) (Machine, error) {
	var r Machine
	var deadline, warm sql.NullInt64
	var created, changed, seen int64
	err := row.Scan(&r.ID, &r.Name, &r.Provider, &r.Template, &r.ProviderRef, &r.State, &r.OS, &r.Arch, &r.Home, &r.Shell,
		&r.SSHHostKey, &r.HelperSHA256, &r.ConfigSHA256, &r.Hardware, &deadline, &warm, &r.Drained, &r.BrokenReason, &created, &changed, &seen)
	r.ProviderDeadline, r.WarmUntil = fromMS(deadline), fromMS(warm)
	if seen > 0 {
		r.LastSeenAt = time.UnixMilli(seen)
	}
	r.CreatedAt, r.StateChangedAt = time.UnixMilli(created), time.UnixMilli(changed)
	return r, err
}

// InsertMachine inserts a machine row with its rooms. If name is empty, the name is
// hostel-<serverID>-<id>, made from the new row ID in the same transaction.
func (s *Store) InsertMachine(ctx context.Context, r Machine, serverID string, rooms []string, now time.Time) (Machine, error) {
	r.CreatedAt, r.StateChangedAt = now, now
	err := s.tx(ctx, func(tx *sql.Tx) error {
		name := r.Name
		if name == "" {
			name = fmt.Sprintf("hostel-%s-pending", serverID)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO machines (name, provider, template, state, os, created_at, state_changed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, name, r.Provider, r.Template, r.State, r.OS, now.UnixMilli(), now.UnixMilli())
		if err != nil {
			return err
		}
		if r.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		if r.Name == "" {
			r.Name = fmt.Sprintf("hostel-%s-%d", serverID, r.ID)
			if _, err := tx.ExecContext(ctx, `UPDATE machines SET name = ? WHERE id = ?`, r.Name, r.ID); err != nil {
				return err
			}
		}
		for _, sl := range rooms {
			if _, err := tx.ExecContext(ctx, `INSERT INTO rooms (machine_id, name, state) VALUES (?, ?, 'free')`, r.ID, sl); err != nil {
				return err
			}
		}
		return insertEvent(ctx, tx, now, "", r.ID, "machine", "machine "+r.Name+" "+r.State)
	})
	if err != nil {
		return Machine{}, fmt.Errorf("insert machine: %w", err)
	}
	return r, nil
}

// EnsureRooms adds the rooms that a machine does not have yet.
func (s *Store) EnsureRooms(ctx context.Context, machineID int64, rooms []string) error {
	for _, sl := range rooms {
		if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO rooms (machine_id, name, state) VALUES (?, ?, 'free')`, machineID, sl); err != nil {
			return fmt.Errorf("insert room: %w", err)
		}
	}
	return nil
}

// ResetRetiredMachine forgets the host key and the inventory of the
// retired machine of a machine: a machine that joins again with a code can be a
// new host (the code proves it).
func (s *Store) ResetRetiredMachine(ctx context.Context, provider, template string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM machines WHERE provider = ? AND template = ? AND state = 'retired'`, provider, template).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE machines SET ssh_host_key = '', helper_sha256 = '', config_sha256 = '' WHERE id = ?`, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM machine_tools WHERE machine_id = ?`, id)
		return err
	})
}

// RemoveFreeRooms deletes the free rooms of a machine that are not in keep (rooms
// removed from the configuration).
func (s *Store) RemoveFreeRooms(ctx context.Context, machineID int64, keep []string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM rooms WHERE machine_id = ? AND state = 'free'`, machineID)
	if err != nil {
		return err
	}
	var gone []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if !slices.Contains(keep, name) {
			gone = append(gone, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, name := range gone {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM rooms WHERE machine_id = ? AND name = ? AND state = 'free'`, machineID, name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Machine(ctx context.Context, id int64) (Machine, error) {
	r, err := scanMachine(s.db.QueryRowContext(ctx, `SELECT `+machineColumns+` FROM machines WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Machine{}, ErrNotFound
	}
	if err != nil {
		return Machine{}, fmt.Errorf("read machine %d: %w", id, err)
	}
	return r, nil
}

// Machines returns all machines that are not destroyed, in ID order.
func (s *Store) Machines(ctx context.Context) ([]Machine, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+machineColumns+` FROM machines WHERE state != 'destroyed' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	defer rows.Close()
	var out []Machine
	for rows.Next() {
		r, err := scanMachine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastHardware returns the hardware of the newest machine of each machine
// ("<provider>/<machine>"), also of a machine that is gone: an on-demand machine
// has no machine most of the time.
func (s *Store) LastHardware(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT provider, template, hardware, MAX(id) FROM machines WHERE hardware != '' GROUP BY provider, template`)
	if err != nil {
		return nil, fmt.Errorf("list machine hardware: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var provider, template, hw string
		var id int64
		// SQLite takes the other columns from the row of MAX(id).
		if err := rows.Scan(&provider, &template, &hw, &id); err != nil {
			return nil, err
		}
		out[provider+"/"+template] = hw
	}
	return out, rows.Err()
}

// SetMachineState changes the state if it is one of from (compare-and-set). It
// reports whether the row changed, and records an event.
func (s *Store) SetMachineState(ctx context.Context, id int64, from []string, to, reason string, now time.Time) (bool, error) {
	changed := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var state, name string
		if err := tx.QueryRowContext(ctx, `SELECT state, name FROM machines WHERE id = ?`, id).Scan(&state, &name); err != nil {
			return err
		}
		if !contains(from, state) {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE machines SET state = ?, broken_reason = ?, state_changed_at = ? WHERE id = ?`,
			to, reason, now.UnixMilli(), id); err != nil {
			return err
		}
		changed = true
		msg := "machine " + name + " " + state + " → " + to
		if reason != "" {
			msg += ": " + reason
		}
		return insertEvent(ctx, tx, now, "", id, "machine", msg)
	})
	if err != nil {
		return false, fmt.Errorf("set machine %d state: %w", id, err)
	}
	return changed, nil
}

// SetMachineProbe records the probe result.
func (s *Store) SetMachineProbe(ctx context.Context, id int64, osName, arch, home, shell string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET os = ?, arch = ?, home = ?, shell = ? WHERE id = ?`, osName, arch, home, shell, id)
	return err
}

// PinHostKey records the machine host key if none is recorded (compare-and-set).
func (s *Store) PinHostKey(ctx context.Context, id int64, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET ssh_host_key = ? WHERE id = ? AND ssh_host_key = ''`, key, id)
	return err
}

func (s *Store) SetHelperSHA256(ctx context.Context, id int64, sum string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET helper_sha256 = ? WHERE id = ?`, sum, id)
	return err
}

// SetLastSeen records that the machine answered a presence check.
func (s *Store) SetLastSeen(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET last_seen_at = ? WHERE id = ?`, now.UnixMilli(), id)
	return err
}

// SetHardware records the hardware of a machine (JSON).
func (s *Store) SetHardware(ctx context.Context, id int64, hw string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET hardware = ? WHERE id = ?`, hw, id)
	return err
}

// SetConfigSHA256 records the hash of the machine settings that a machine has.
func (s *Store) SetConfigSHA256(ctx context.Context, id int64, sum string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET config_sha256 = ? WHERE id = ?`, sum, id)
	return err
}

// SetRoomHome records the home directory of a room's own user.
func (s *Store) SetRoomHome(ctx context.Context, machineID int64, room, home string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rooms SET home = ? WHERE machine_id = ? AND name = ?`, home, machineID, room)
	return err
}

// SetLeaseRoomsUser records the lease user of a lease's rooms and its home
// directory (M11).
func (s *Store) SetLeaseRoomsUser(ctx context.Context, leaseID, user, home string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rooms SET lease_user = ?, home = ? WHERE `+leaseRooms, user, home, leaseID, leaseID)
	return err
}

// ClearLeaseUser removes a deleted lease user from the rooms of a machine.
func (s *Store) ClearLeaseUser(ctx context.Context, machineID int64, user string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rooms SET lease_user = '', home = '' WHERE machine_id = ? AND lease_user = ?`, machineID, user)
	return err
}

// RoomLeaseUser returns the lease user of a room, or "".
func (s *Store) RoomLeaseUser(ctx context.Context, machineID int64, room string) (string, error) {
	var u string
	err := s.db.QueryRowContext(ctx, `SELECT lease_user FROM rooms WHERE machine_id = ? AND name = ?`, machineID, room).Scan(&u)
	return u, err
}

// RoomHome returns the recorded home directory of a room's own user.
func (s *Store) RoomHome(ctx context.Context, machineID int64, room string) (string, error) {
	var home string
	err := s.db.QueryRowContext(ctx, `SELECT home FROM rooms WHERE machine_id = ? AND name = ?`, machineID, room).Scan(&home)
	return home, err
}

// Rooms returns the rooms of all machines that are not destroyed.
func (s *Store) Rooms(ctx context.Context) ([]Room, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT s.machine_id, s.name, s.state, s.lease_id, s.cleanup_data, s.home, s.lease_user
		FROM rooms s JOIN machines r ON r.id = s.machine_id WHERE r.state != 'destroyed' ORDER BY s.machine_id, s.name`)
	if err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}
	defer rows.Close()
	var out []Room
	for rows.Next() {
		var sl Room
		if err := rows.Scan(&sl.MachineID, &sl.Name, &sl.State, &sl.LeaseID, &sl.CleanupData, &sl.Home, &sl.LeaseUser); err != nil {
			return nil, err
		}
		out = append(out, sl)
	}
	return out, rows.Err()
}

// MachineTools returns the inventory rows of all machines.
func (s *Store) MachineTools(ctx context.Context) ([]MachineTool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT machine_id, tool, scripts_sha256, state, option, env, checked_at FROM machine_tools ORDER BY machine_id, tool`)
	if err != nil {
		return nil, fmt.Errorf("list machine tools: %w", err)
	}
	defer rows.Close()
	var out []MachineTool
	for rows.Next() {
		var rt MachineTool
		var env string
		var at int64
		if err := rows.Scan(&rt.MachineID, &rt.Tool, &rt.ScriptsSHA256, &rt.State, &rt.Option, &env, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(env), &rt.Env); err != nil {
			return nil, fmt.Errorf("machine tool %s: env: %w", rt.Tool, err)
		}
		rt.CheckedAt = time.UnixMilli(at)
		out = append(out, rt)
	}
	return out, rows.Err()
}

// SetMachineTool writes the inventory row of a tool on a machine.
func (s *Store) SetMachineTool(ctx context.Context, rt MachineTool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO machine_tools (machine_id, tool, scripts_sha256, state, option, env, checked_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (machine_id, tool) DO UPDATE SET scripts_sha256 = excluded.scripts_sha256, state = excluded.state,
		option = excluded.option, env = excluded.env, checked_at = excluded.checked_at`,
		rt.MachineID, rt.Tool, rt.ScriptsSHA256, rt.State, rt.Option, jsonText(nonNil(rt.Env)), rt.CheckedAt.UnixMilli())
	return err
}

// RemoveMachineTools deletes the inventory rows of a machine whose tool is not in
// keep (tools that its machine no longer lists).
func (s *Store) RemoveMachineTools(ctx context.Context, machineID int64, keep []string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM machine_tools WHERE machine_id = ? AND tool NOT IN (SELECT value FROM json_each(?))`,
		machineID, jsonText(nonNil(keep)))
	return err
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ms converts a time to Unix milliseconds; the zero time is NULL.
func ms(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// fromMS converts nullable Unix milliseconds to a time; NULL is the zero time.
func fromMS(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64)
}

// SetProviderRef records the provider's ID of a machine and its provider deadline.
func (s *Store) SetProviderRef(ctx context.Context, id int64, ref string, deadline time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET provider_ref = ?, provider_deadline = ? WHERE id = ?`, ref, ms(deadline), id)
	return err
}

// SetProviderDeadline records an extended provider deadline.
func (s *Store) SetProviderDeadline(ctx context.Context, id int64, deadline time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET provider_deadline = ? WHERE id = ?`, ms(deadline), id)
	return err
}

// SetWarmUntil records when an idle machine stops or is destroyed (zero: not idle).
// SetMachineReady records the first time that a machine is running.
func (s *Store) SetMachineReady(ctx context.Context, id int64, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET ready_at = ? WHERE id = ? AND ready_at IS NULL`, ms(t), id)
	return err
}

// RecentCreationTimes returns the creation times (from the row to the first
// running state) of the last n machines of the machines.
func (s *Store) RecentCreationTimes(ctx context.Context, templates []string, n int) ([]time.Duration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ready_at - created_at FROM machines WHERE ready_at IS NOT NULL
		AND provider || '/' || template IN (SELECT value FROM json_each(?)) ORDER BY ready_at DESC LIMIT ?`, jsonText(nonNil(templates)), n)
	if err != nil {
		return nil, fmt.Errorf("read machine creation times: %w", err)
	}
	defer rows.Close()
	var out []time.Duration
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, time.Duration(d)*time.Millisecond)
	}
	return out, rows.Err()
}

func (s *Store) SetWarmUntil(ctx context.Context, id int64, t time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET warm_until = ? WHERE id = ?`, ms(t), id)
	return err
}

func (s *Store) SetDrained(ctx context.Context, id int64, drained bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET drained = ? WHERE id = ?`, drained, id)
	return err
}

// ClearHostKey removes the pinned host key (admin machine repair --accept-host-key).
func (s *Store) ClearHostKey(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE machines SET ssh_host_key = '' WHERE id = ?`, id)
	return err
}

// SetRoom changes one room.
func (s *Store) SetRoom(ctx context.Context, machineID int64, name, state, leaseID, cleanupData string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rooms SET state = ?, lease_id = ?, cleanup_data = ? WHERE machine_id = ? AND name = ?`,
		state, leaseID, cleanupData, machineID, name)
	return err
}

// BreakRooms marks the broken rooms of a machine dirty, so the next probe runs
// their cleanup again (admin machine repair).
func (s *Store) BreakRooms(ctx context.Context, machineID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE rooms SET state = 'dirty' WHERE machine_id = ? AND state = 'broken' AND cleanup_data != ''`, machineID)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE rooms SET state = 'free', lease_id = '' WHERE machine_id = ? AND state = 'broken'`, machineID)
	return err
}
