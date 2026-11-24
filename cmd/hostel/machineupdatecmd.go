package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/protocol"
)

// machinesUpdateWait bounds the wait for remote updates: the cleanup of the
// ended leases, the update, and the new connection of each machine.
const machinesUpdateWait = 30 * time.Minute

// machinesUpdate is "hostel config machines update" (M13): the server ends
// the leases of each machine, updates it to its release, and starts it again.
func machinesUpdate(names []string, all, wait bool, stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	var started []protocol.MachineUpdate
	code := 0
	if all {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		var out protocol.MachineUpdates
		err := c.Do(ctx, "POST", "/v1/machines/update", nil, &out)
		cancel()
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		for _, sk := range out.Skipped {
			fmt.Fprintf(stdout, "Skipped %s.\n", sk)
		}
		started = out.Started
	} else {
		st, err := getStatus(c)
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		for _, name := range names {
			m, err := findMachine(st, name)
			if err != nil {
				fmt.Fprintln(stderr, "hostel:", err)
				code = 1
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			var u protocol.MachineUpdate
			err = c.Do(ctx, "POST", fmt.Sprintf("/v1/machines/%d/update", m.ID), nil, &u)
			cancel()
			var apiErr *client.APIError
			switch {
			case errors.As(err, &apiErr) && apiErr.Code == "up_to_date":
				fmt.Fprintf(stdout, "Up to date: %s.\n", apiErr.Message)
			case err != nil:
				fmt.Fprintf(stderr, "hostel: machine %s: %v\n", m.Name, err)
				code = 1
			default:
				started = append(started, u)
			}
		}
	}
	for _, u := range started {
		fmt.Fprintf(stdout, "Machine %s: update to %s started; its active leases end.\n", u.Machine, u.To)
	}
	if !wait || len(started) == 0 {
		return code
	}
	waiting := map[string]string{}
	for _, u := range started {
		waiting[u.Machine] = u.To
	}
	end := time.Now().Add(machinesUpdateWait)
	for len(waiting) > 0 {
		if time.Now().After(end) {
			for name := range waiting {
				fmt.Fprintf(stderr, "hostel: machine %s: the update did not end in %s; \"hostel status %s\" shows it\n", name, machinesUpdateWait, name)
			}
			return 1
		}
		time.Sleep(2 * time.Second)
		st, err := getStatus(c)
		if err != nil {
			continue
		}
		for _, m := range st.Machines {
			to, ok := waiting[m.Name]
			if !ok || m.UpdateTo != "" || m.State == protocol.MachineUpdating {
				continue
			}
			delete(waiting, m.Name)
			if m.State == protocol.MachineBroken {
				fmt.Fprintf(stderr, "hostel: machine %s: %s\n", m.Name, m.Reason)
				code = 1
				continue
			}
			fmt.Fprintf(stdout, "Machine %s: updated to %s.\n", m.Name, to)
		}
	}
	return code
}
