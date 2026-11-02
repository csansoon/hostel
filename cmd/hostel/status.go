package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

func apiClient() (*client.Client, error) {
	g, err := config.LoadGuest()
	if err != nil {
		return nil, err
	}
	return client.New(g)
}

func statusCmd(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel status", stderr)
	if _, err := cli.Parse(fs, args); err != nil {
		return 2
	}
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	var st protocol.Status
	if err := c.Do(context.Background(), "GET", "/v1/status", nil, &st); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "LEASE\tSTATE\tPLATFORM\tDEVICES\tPRIORITY\tUSER\tGUEST\tROOM\tAGE\tEXPIRES\tTASK")
	for _, l := range st.Leases {
		state := l.State
		if l.State == protocol.LeaseQueued {
			state = fmt.Sprintf("queued #%d (%s)", l.Position, l.Reason)
		}
		room, expires := "-", "-"
		if l.RoomID != 0 {
			room = fmt.Sprintf("%d/%s", l.RoomID, l.Slot)
		}
		if !l.ExpiresAt.IsZero() {
			expires = l.ExpiresAt.Local().Format(time.TimeOnly)
		}
		user := l.User
		if user == "" {
			user = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", l.ID, state, l.Platform, strings.Join(l.Devices, ","),
			l.Priority, user, l.Guest+":"+l.Repo, room, time.Since(l.CreatedAt).Round(time.Second), expires, l.Task)
	}
	tw.Flush()
	fmt.Fprintln(stdout)
	tw = tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ROOM\tNAME\tMACHINE\tOS\tSTATE\tSLOTS\tPLATFORMS")
	for _, r := range st.Rooms {
		var slots []string
		for _, sl := range r.Slots {
			s := sl.Name + ":" + sl.State
			if sl.LeaseID != "" {
				s += "(" + sl.LeaseID + ")"
			}
			slots = append(slots, s)
		}
		state := r.State
		if r.Reason != "" {
			state += " (" + r.Reason + ")"
		}
		if r.Drained {
			state += " drained"
		}
		osArch := r.OS
		if r.Arch != "" {
			osArch += "/" + r.Arch
		}
		fmt.Fprintf(tw, "%d\t%s\t%s/%s\t%s\t%s\t%s\t%s\n", r.ID, r.Name, r.Provider, r.Machine, osArch, state,
			strings.Join(slots, " "), strings.Join(r.Platforms, ","))
	}
	tw.Flush()
	return 0
}
