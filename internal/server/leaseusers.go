package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/machineops"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/store"
)

// Lease users (M11, design section 18): on a machine with lease_users, a lease
// whose tools do not need the machine user gets its own OS user. The root
// command runs through a sudo rule that allows only it.

// leaseUserRoot is the root-owned binary that "hostel machine join" installs.
const leaseUserRoot = "/etc/hostel/hostel"

// leaseUserVersion is the version of the root step that the server needs.
const leaseUserVersion = 1

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
func rootLeaseUser(ctx context.Context, conn *machineops.Conn, in, out any, args ...string) error {
	var stdin *bytes.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		stdin = bytes.NewReader(data)
	} else {
		stdin = bytes.NewReader(nil)
	}
	var stdout bytes.Buffer
	cmd := "sudo -n " + leaseUserRoot + " lease-user " + strings.Join(args, " ")
	code, stderr, err := conn.Run(ctx, cmd, stdin, &stdout)
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
	if err := rootLeaseUser(ctx, conn, nil, &c, "check"); err != nil {
		return err
	}
	if c.Version < leaseUserVersion {
		return fmt.Errorf("the root step of lease users has version %d, the server needs %d: run \"hostel machine join\" again on the machine", c.Version, leaseUserVersion)
	}
	return nil
}

// ensureLeaseUser creates the lease user of l and records it for the lease's
// rooms. The name is recorded first, so that cleanup deletes a user that a
// failed step left; create of an existing lease user changes nothing.
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
	if err := s.store.SetLeaseRoomsUser(ctx, l.ID, name, ""); err != nil {
		return err
	}
	conn, err := s.connect(ctx, r)
	if err != nil {
		return err
	}
	defer conn.Close()
	var created protocol.LeaseUserCreated
	req := protocol.LeaseUserCreate{PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.serverKey.PublicKey())))}
	if err := rootLeaseUser(ctx, conn, req, &created, "create", name); err != nil {
		return fmt.Errorf("create the lease user: %w", err)
	}
	if created.Home == "" {
		return fmt.Errorf("create the lease user: no home directory in the answer")
	}
	return s.store.SetLeaseRoomsUser(ctx, l.ID, name, created.Home)
}

// deleteLeaseUsers deletes, as root, every lease user on the machine that no
// room of the machine records except the one in room (whose cleanup ended),
// and clears that room's record. Users of earlier failed steps go too.
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
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, sl := range rooms {
		if sl.MachineID == machineID && sl.LeaseUser != "" && sl.LeaseUser != done {
			keep[sl.LeaseUser] = true
		}
	}
	conn, err := s.connect(ctx, r)
	if err != nil {
		return err
	}
	defer conn.Close()
	var list protocol.LeaseUserList
	if err := rootLeaseUser(ctx, conn, nil, &list, "list"); err != nil {
		return err
	}
	for _, u := range list.Users {
		if keep[u] {
			continue
		}
		if err := rootLeaseUser(ctx, conn, nil, nil, "delete", u); err != nil {
			return fmt.Errorf("delete the lease user %s: %w", u, err)
		}
	}
	if done == "" {
		return nil
	}
	return s.store.ClearLeaseUser(ctx, machineID, done)
}
