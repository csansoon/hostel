package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/machineops"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/shell"
	"github.com/csansoon/hostel/internal/store"
)

// fillLeaseHome runs as the lease user: its own hostel directory with copies
// of the machine user's helper, mise, tools, and mise configuration, its own
// cache with links to the machine's shared cache entries (not mise's own
// directories), and the rooms directory. $1 is the machine user's hostel
// directory.
const fillLeaseHome = `set -e
m="$1"; d="$HOME/hostel"
mkdir -p "$d/cache" "$d/rooms"
for x in bin tools mise; do
	rm -rf "$d/$x"
	if [ -e "$m/$x" ]; then cp -R "$m/$x" "$d/$x"; fi
done
for e in "$m"/cache/*; do
	[ -e "$e" ] || continue
	n=$(basename "$e")
	case "$n" in mise|mise-cache) continue;; esac
	[ -e "$d/cache/$n" ] || [ -L "$d/cache/$n" ] || ln -s "$e" "$d/cache/$n"
done`

// Lease users (M11, design section 18): on a machine with lease_users, a lease
// whose tools do not need the machine user gets its own OS user. The root
// command runs through a sudo rule that allows only it.

// leaseUserRoot is the root-owned binary that "hostel machine join" installs.
const leaseUserRoot = "/etc/hostel/hostel"

// leaseUserVersion is the version of the root step that the server needs.
const leaseUserVersion = 2

func leaseUserName(leaseID string) string { return "hostel-" + leaseID[:12] }

// wantsLeaseUser reports whether a lease on a machine of m gets a lease user.
func wantsLeaseUser(cfg *config.Server, m *config.Template, l store.Lease) bool {
	if m == nil || !m.LeaseUsers {
		return false
	}
	for _, t := range leaseClosure(cfg, l.Tools) {
		if t.MachineUser {
			return false
		}
	}
	return true
}

// rootLeaseUser runs "hostel lease-user <args>" as root through sudo, as the
// machine user, and decodes its JSON output into out.
func rootLeaseUser(ctx context.Context, conn *machineops.Conn, out any, args ...string) error {
	var stdout bytes.Buffer
	cmd := "sudo -n " + leaseUserRoot + " lease-user " + strings.Join(args, " ")
	code, stderr, err := conn.Run(ctx, cmd, bytes.NewReader(nil), &stdout)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("%s (exit code %d): %s; a machine with lease_users needs the root step (docs/machine-setup.md, lease users)",
			cmd, code, strings.TrimSpace(stderr))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(stdout.Bytes(), out)
}

// checkLeaseUserRoot checks the root step at provisioning.
func checkLeaseUserRoot(ctx context.Context, conn *machineops.Conn) error {
	var c protocol.LeaseUserCheck
	if err := rootLeaseUser(ctx, conn, &c, "check"); err != nil {
		return err
	}
	if c.Version < leaseUserVersion {
		return fmt.Errorf("the root step of lease users has version %d, the server needs %d: run \"hostel machine join\" again on the machine", c.Version, leaseUserVersion)
	}
	return nil
}

// ensureLeaseUser creates the lease user of l and records it for the lease's
// rooms. The name is recorded first, so that cleanup deletes a user that a
// failed step left; create of an existing lease user makes it again.
func (s *Server) ensureLeaseUser(ctx context.Context, l store.Lease) error {
	r, err := s.store.Machine(ctx, l.MachineID)
	if err != nil {
		return err
	}
	lc := s.live.Load()
	m := lc.cfg.Template(r.Provider + "/" + r.Template)
	if !wantsLeaseUser(lc.cfg, m, l) {
		return nil
	}
	name := leaseUserName(l.ID)
	if u, err := s.store.RoomLeaseUser(ctx, r.ID, l.RoomName); err != nil {
		return err
	} else if u == name {
		if home, err := s.store.RoomHome(ctx, r.ID, l.RoomName); err == nil && home != "" {
			return nil
		}
	}
	// One lease-user change at a time: the sweep of another cleanup must not
	// delete a user between its record and its creation.
	s.leaseUserMu.Lock()
	defer s.leaseUserMu.Unlock()
	if err := s.store.SetLeaseRoomsUser(ctx, l.ID, name, ""); err != nil {
		return err
	}
	machine, _, err := s.openMachine(ctx, r.ID)
	if err != nil {
		return err
	}
	defer machine.Conn.Close()
	var created protocol.LeaseUserCreated
	if err := rootLeaseUser(ctx, machine.Conn, &created, "create", name); err != nil {
		return fmt.Errorf("create the lease user: %w", err)
	}
	if created.Home == "" {
		return fmt.Errorf("create the lease user: no home directory in the answer")
	}
	// After create, in a new login: create adds the machine user to the
	// group of lease users.
	sharer, _, err := s.openMachine(ctx, r.ID)
	if err != nil {
		return err
	}
	defer sharer.Conn.Close()
	if err := sharer.Helper(ctx, []string{"share-cache"}, nil, nil); err != nil {
		return fmt.Errorf("share the machine's cache: %w", err)
	}
	conn, err := s.connectAs(ctx, r, name)
	if err != nil {
		return err
	}
	defer conn.Close()
	cmd, err := shell.Command(shell.SH, "/bin/sh", "-c", fillLeaseHome, "sh", machineops.Path(r.OS, r.Home))
	if err != nil {
		return err
	}
	if code, stderr, err := conn.Run(ctx, cmd, bytes.NewReader(nil), nil); err != nil || code != 0 {
		return fmt.Errorf("fill the home directory of the lease user (exit code %d): %v %s", code, err, strings.TrimSpace(stderr))
	}
	return s.store.SetLeaseRoomsUser(ctx, l.ID, name, created.Home)
}

// deleteLeaseUsers deletes, as root, every lease user on the machine that no
// active room of the machine records except the one in room (whose cleanup
// ended), and clears the records of the deleted ones. Users of earlier failed
// steps go too.
func (s *Server) deleteLeaseUsers(ctx context.Context, machineID int64, room string) error {
	r, err := s.store.Machine(ctx, machineID)
	if err != nil {
		return err
	}
	m := s.config().Template(r.Provider + "/" + r.Template)
	var done string
	if room != "" {
		if done, err = s.store.RoomLeaseUser(ctx, machineID, room); err != nil {
			return err
		}
	}
	if done == "" && (m == nil || !m.LeaseUsers) {
		return nil
	}
	s.leaseUserMu.Lock()
	defer s.leaseUserMu.Unlock()
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, sl := range rooms {
		if sl.MachineID == machineID && sl.LeaseUser != "" && sl.LeaseUser != done && sl.State != protocol.RoomFree {
			keep[sl.LeaseUser] = true
		}
	}
	machine, _, err := s.openMachine(ctx, r.ID)
	if err != nil {
		return err
	}
	defer machine.Conn.Close()
	var list protocol.LeaseUserList
	if err := rootLeaseUser(ctx, machine.Conn, &list, "list"); err != nil {
		return err
	}
	for _, u := range list.Users {
		if keep[u] {
			continue
		}
		if err := rootLeaseUser(ctx, machine.Conn, nil, "delete", u); err != nil {
			return fmt.Errorf("delete the lease user %s: %w", u, err)
		}
		if err := s.store.ClearLeaseUser(ctx, machineID, u); err != nil {
			return err
		}
	}
	if done == "" {
		return nil
	}
	return s.store.ClearLeaseUser(ctx, machineID, done)
}

// deleteLeaseUserInstead handles a failed login as a room's user: a lease user
// can break its own login (its authorized_keys), and its deletion removes all
// of it. For other users it returns err.
func (s *Server) deleteLeaseUserInstead(ctx context.Context, machineID int64, room string, err error) error {
	u, uerr := s.store.RoomLeaseUser(ctx, machineID, room)
	if uerr != nil || u == "" {
		return err
	}
	s.log.Warn("no login as the lease user; the user is deleted", "machine_id", machineID, "room", room, "err", err)
	return s.deleteLeaseUsers(ctx, machineID, room)
}
