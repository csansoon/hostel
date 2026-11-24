package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/shell"
	"github.com/csansoon/hostel/internal/store"
	"github.com/csansoon/hostel/internal/update"
)

// machineUpdateWait bounds the root step of a remote update and the new
// connection of the machine with the new version (M13).
const machineUpdateWait = 10 * time.Minute

// errUpToDate: the machine's connection has the server's release already, or
// a newer one.
var errUpToDate = errors.New("up to date")

// needsUpdate: a connection with version have needs the release tag (a
// build that is not a release, such as dev, gets it too).
func needsUpdate(have, tag string) bool {
	if have == tag {
		return false
	}
	_, known := update.Parse(have)
	return !known || update.Older(have, tag)
}

// updateReason is the end reason of the leases that an update ends.
func updateReason(machine string) string {
	return fmt.Sprintf("the machine %s is updated; acquire again", machine)
}

// serverRelease is the signed release of helper_dir (update.Server writes
// it), checked with the release key of this server.
func (s *Server) serverRelease() (*update.Release, error) {
	dir := s.config().HelperDir
	files := map[string][]byte{}
	for _, name := range []string{update.TagFile, update.SumsFile, update.SigFile} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("the server has no signed release in its helper_dir (%s is missing): update the server to a release first (hostel server update), or update the machine by hand (hostel machine update, as an administrator on it)", name)
		}
		files[name] = data
	}
	src, err := update.Default()
	if err != nil {
		return nil, err
	}
	return update.Verify(src.Key, strings.TrimSpace(string(files[update.TagFile])), files[update.SumsFile], files[update.SigFile])
}

// handleMachineUpdate is POST /v1/machines/{id}/update (admin, M13): Hostel
// ends the active leases of a machine that connects, updates its software to
// the server's release, and lets it take leases again.
func (s *Server) handleMachineUpdate(w http.ResponseWriter, r *http.Request) {
	machine, ok := s.machineFor(w, r)
	if !ok {
		return
	}
	tag, err := s.startMachineUpdate(r.Context(), machine, requestToken(r).Name)
	switch {
	case errors.Is(err, errUpToDate):
		writeError(w, http.StatusConflict, "up_to_date", fmt.Sprintf("machine %s has %s already", machine.Name, tag))
		return
	case err != nil:
		writeError(w, http.StatusConflict, "", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.MachineUpdate{Machine: machine.Name, To: tag})
}

// startMachineUpdate records the update, drains the machine, and ends its
// active leases; the loop starts the update when its rooms are free.
func (s *Server) startMachineUpdate(ctx context.Context, machine store.Machine, admin string) (string, error) {
	m, err := s.templateOf(machine)
	if err != nil {
		return "", err
	}
	if !m.Connect {
		return "", fmt.Errorf("machine %s does not connect to the server (connect = false): Hostel updates only machines that joined; update it by hand", machine.Name)
	}
	if machine.UpdateTo != "" {
		return machine.UpdateTo, nil
	}
	// The server's own update replaces helper_dir: a machine must not read
	// a mix of two releases.
	if s.serverUpdating() {
		return "", errors.New("the server updates itself now; update the machines when it runs the new version")
	}
	rel, err := s.serverRelease()
	if err != nil {
		return "", err
	}
	have := s.machines.Version(m.Key())
	switch {
	case have != "" && !needsUpdate(have, rel.Tag):
		return have, errUpToDate
	case machine.State != protocol.MachineRunning:
		return "", fmt.Errorf("machine %s is %s; Hostel updates a running machine (repair it first)", machine.Name, machine.State)
	}
	started, err := s.store.StartMachineUpdate(ctx, machine.ID, rel.Tag)
	if err != nil {
		return "", err
	}
	if !started {
		// Another request started an update just now: its release.
		cur, err := s.store.Machine(ctx, machine.ID)
		if err != nil {
			return "", err
		}
		return cur.UpdateTo, nil
	}
	s.store.AddEvent(ctx, time.Now(), "", machine.ID, "admin", fmt.Sprintf("machine %s: update from %s to %s by %s", machine.Name, have, rel.Tag, admin))
	// Also each loop pass (startUpdates): a lease that the scheduler
	// assigned meanwhile, and after a restart.
	for _, l := range s.machineLeases(ctx, machine.ID) {
		if err := s.endLease(ctx, l.ID, updateReason(machine.Name)); err != nil {
			s.log.Warn("machine update: end a lease", "lease", l.ID, "err", err)
		}
	}
	s.kickScheduler()
	return rel.Tag, nil
}

// windowsServiceBinary is the binary of the SYSTEM service on Windows, the
// pipe client of the root step: the machine user cannot read the directory
// of the connect service (C:\ProgramData\hostel).
const windowsServiceBinary = `C:\Program Files\Hostel\hostel.exe`

// updateMachinesFile, in data_dir, asks the server that starts after a server
// update to update its machines (POST /v1/server/update with "machines").
const updateMachinesFile = "update-machines"

// updateMachinesTime is how long a server after its update waits for its
// machines to connect, to update them.
const updateMachinesTime = 15 * time.Minute

// handleUpdateMachines is POST /v1/machines/update (admin, M13): an update
// of each machine that connects with another version than the server's
// release.
func (s *Server) handleUpdateMachines(w http.ResponseWriter, r *http.Request) {
	if _, err := s.serverRelease(); err != nil {
		writeError(w, http.StatusConflict, "", err.Error())
		return
	}
	out, err := s.updateMachines(r.Context(), requestToken(r).Name, false)
	if err != nil {
		s.internalError(w, "update machines", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// updateMachines starts the update of each machine that connects with an
// older version; quiet skips the machines that are not connected or not
// running without a reason (the updates after a server update).
func (s *Server) updateMachines(ctx context.Context, admin string, quiet bool) (protocol.MachineUpdates, error) {
	out := protocol.MachineUpdates{Started: []protocol.MachineUpdate{}, Skipped: []string{}}
	machines, err := s.store.Machines(ctx)
	if err != nil {
		return out, err
	}
	for _, r := range machines {
		m := s.config().Template(r.Provider + "/" + r.Template)
		if m == nil || !m.Connect || r.State == protocol.MachineRetired {
			continue
		}
		if s.machines.Version(m.Key()) == "" {
			if !quiet {
				out.Skipped = append(out.Skipped, r.Name+": not connected")
			}
			continue
		}
		if quiet && (r.State != protocol.MachineRunning || r.UpdateTo != "") {
			continue
		}
		tag, err := s.startMachineUpdate(ctx, r, admin)
		switch {
		case errors.Is(err, errUpToDate):
			if !quiet {
				out.Skipped = append(out.Skipped, r.Name+": has "+tag+" already")
			}
		case err != nil:
			out.Skipped = append(out.Skipped, r.Name+": "+err.Error())
		default:
			out.Started = append(out.Started, protocol.MachineUpdate{Machine: r.Name, To: tag})
		}
	}
	return out, nil
}

// updateMachinesAfterUpdate (the loop pass): after a server update with
// "machines", it updates each machine that connects with an older version,
// for updateMachinesTime after the start.
func (s *Server) updateMachinesAfterUpdate(ctx context.Context) {
	file := filepath.Join(s.config().DataDir, updateMachinesFile)
	if !s.updateMachinesChecked {
		// The file stays for the whole time: a second restart continues.
		s.updateMachinesChecked = true
		if fi, err := os.Stat(file); err == nil {
			data, _ := os.ReadFile(file)
			s.updateMachinesBy = strings.TrimSpace(string(data))
			s.updateMachinesUntil = fi.ModTime().Add(updateMachinesTime)
			s.log.Info("machine updates after the server update", "by", s.updateMachinesBy, "until", s.updateMachinesUntil)
		}
	}
	if s.updateMachinesBy == "" {
		return
	}
	if time.Now().After(s.updateMachinesUntil) {
		os.Remove(file)
		s.updateMachinesBy = ""
		return
	}
	if _, err := s.serverRelease(); err != nil {
		s.log.Warn("update machines after the server update", "err", err)
		os.Remove(file)
		s.updateMachinesBy = ""
		return
	}
	if out, err := s.updateMachines(ctx, s.updateMachinesBy, true); err == nil {
		for _, u := range out.Started {
			s.log.Info("machine update after the server update", "machine", u.Machine, "to", u.To)
		}
	}
}

// startUpdates moves each running machine with an update and no busy room to
// updating (the loop pass).
func (s *Server) startUpdates(ctx context.Context) error {
	s.updateMachinesAfterUpdate(ctx)
	machines, err := s.store.Machines(ctx)
	if err != nil {
		return err
	}
	rooms, err := s.store.Rooms(ctx)
	if err != nil {
		return err
	}
	busy := map[int64]bool{}
	for _, sl := range rooms {
		switch sl.State {
		case protocol.RoomAssigned, protocol.RoomPreparing, protocol.RoomLeased, protocol.RoomCleaning:
			busy[sl.MachineID] = true
		}
	}
	for _, r := range machines {
		if r.UpdateTo == "" {
			continue
		}
		switch r.State {
		case protocol.MachineRunning, protocol.MachineUpdating, protocol.MachineProvisioning:
		default:
			// The machine left running before its update started (a failed
			// cleanup, a deallocation): the update does not wait forever,
			// and the machine gets its drain state back.
			if ok, _ := s.store.CancelMachineUpdate(ctx, r.ID); ok {
				s.store.AddEvent(ctx, time.Now(), "", r.ID, "machine", fmt.Sprintf("machine %s: update to %s cancelled: the machine is %s; update it again when it runs", r.Name, r.UpdateTo, r.State))
			}
			continue
		}
		if r.State != protocol.MachineRunning {
			continue
		}
		if !busy[r.ID] {
			s.store.SetMachineState(ctx, r.ID, []string{protocol.MachineRunning}, protocol.MachineUpdating, "update to "+r.UpdateTo, time.Now())
			continue
		}
		for _, l := range s.machineLeases(ctx, r.ID) {
			if l.State != protocol.LeaseCleaning {
				if err := s.endLease(ctx, l.ID, updateReason(r.Name)); err != nil {
					s.log.Warn("machine update: end a lease", "lease", l.ID, "err", err)
				}
			}
		}
	}
	return nil
}

// stepUpdating runs the root step on the machine when it connects with an
// older version, and waits until it connects with the new version; then the
// machine provisions again (the new helper) and gets its drain state of
// before. Safe to repeat: the root step on a machine that has the release
// only restarts its connection. A step that loses its connection (the
// restart of the service) is not a failure; at most 3 steps run.
func (s *Server) stepUpdating(ctx context.Context, r store.Machine, m *config.Template) {
	fail := func(err error) {
		s.log.Warn("machine update failed", "machine", r.Name, "err", err)
		s.store.FinishMachineUpdate(ctx, r.ID, protocol.MachineBroken, "update failed: "+err.Error(), time.Now())
	}
	if r.UpdateTo == "" {
		// Nothing to wait for (a row from an interrupted end).
		s.store.FinishMachineUpdate(ctx, r.ID, protocol.MachineProvisioning, "", time.Now())
		return
	}
	cctx, cancel := context.WithTimeout(ctx, machineUpdateWait)
	defer cancel()
	var stepErr error
	var last time.Time
	steps := 0
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		have := s.machines.Version(m.Key())
		if have != "" && !needsUpdate(have, r.UpdateTo) {
			break
		}
		if have != "" && steps < 3 && time.Since(last) > 30*time.Second {
			steps++
			last = time.Now()
			code, out, err := s.runUpdateStep(cctx, r.ID)
			switch {
			case ctx.Err() != nil:
				return
			case err == nil && code != 0:
				fail(fmt.Errorf("the update step on the machine (exit code %d): %s; a machine without the step of remote updates needs \"hostel machine update\" as an administrator on it (two times from a version before v0.5.0)", code, out))
				return
			}
			// The connection ends when the service restarts with the new
			// binary.
			stepErr = err
		}
		select {
		case <-ctx.Done():
			return
		case <-cctx.Done():
			msg := fmt.Sprintf("the machine did not connect with %s in %s", r.UpdateTo, machineUpdateWait)
			if stepErr != nil {
				msg += fmt.Sprintf(" (the update step: %v)", stepErr)
			}
			fail(errors.New(msg))
			return
		case <-tick.C:
		}
	}
	if ok, _ := s.store.FinishMachineUpdate(ctx, r.ID, protocol.MachineProvisioning, "", time.Now()); ok {
		s.store.AddEvent(ctx, time.Now(), "", r.ID, "machine", fmt.Sprintf("machine %s: updated to %s", r.Name, r.UpdateTo))
	}
}

// runUpdateStep runs "hostel machine update --signed" on the machine as the
// machine user: through the sudo rule (macOS, Linux), or the helper as the
// client of the SYSTEM service (Windows). out is the last line of its error
// output.
func (s *Server) runUpdateStep(ctx context.Context, machineID int64) (int, string, error) {
	machine, _, err := s.openMachine(ctx, machineID)
	if err != nil {
		return 0, "", err
	}
	defer machine.Conn.Close()
	cmd := "sudo -n " + leaseUserRoot + " machine update --signed"
	if machine.OS == config.OSWindows {
		if cmd, err = shell.Command(machine.Shell, windowsServiceBinary, "machine", "update", "--signed"); err != nil {
			return 0, "", err
		}
	}
	var stdout bytes.Buffer
	code, stderr, err := machine.Conn.Run(ctx, cmd, bytes.NewReader(nil), &stdout)
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	return code, strings.TrimSpace(lines[len(lines)-1]), err
}
