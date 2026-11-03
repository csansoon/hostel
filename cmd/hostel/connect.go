package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/connect"
	"github.com/csansoon/hostel/internal/invite"
	"github.com/csansoon/hostel/internal/protocol"
)

// ExitNotConnected: the client has no connection (EX_CONFIG).
const ExitNotConnected = 78

func connectCmd(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel connect", stderr)
	name := fs.String("name", "", "your name on the server (default: git config user.name)")
	yes := fs.Bool("yes", false, "replace an existing connection without a question")
	pos, err := cli.Parse(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(stderr, "hostel: usage: hostel connect <invite code> [--name \"Your Name\"]")
		return 2
	}
	code, err := invite.Decode(pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 2
	}
	interactive := isTerminal(os.Stdin)
	in := bufio.NewScanner(os.Stdin)
	old, err := config.ReadGuest()
	if err != nil || old.Token == "" || old.URL() == "" {
		old = nil
	}
	if old != nil && !*yes {
		if !interactive {
			fmt.Fprintf(stderr, "hostel: this computer is connected to %s already; add --yes to replace that connection\n", old.URL())
			return 1
		}
		fmt.Fprintf(stdout, "This computer is connected to %s. Replace that connection with %s? [y/N] ", old.URL(), code.Addresses[0])
		if !in.Scan() || !strings.EqualFold(strings.TrimSpace(in.Text()), "y") {
			fmt.Fprintln(stdout, "Nothing changed.")
			return 1
		}
	}
	if *name == "" {
		*name = connect.GitUserName()
		if interactive {
			fmt.Fprintf(stdout, "Your name [%s]: ", *name)
			if in.Scan() && strings.TrimSpace(in.Text()) != "" {
				*name = strings.TrimSpace(in.Text())
			}
		}
	}
	if *name == "" {
		fmt.Fprintln(stderr, "hostel: give your name: hostel connect <code> --name \"Your Name\"")
		return 2
	}
	res, err := connect.Connect(context.Background(), code, *name)
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	g, resp := res.Guest, res.Response
	// The replaced connection's token must not stay valid.
	if old != nil {
		if err := connect.Revoke(context.Background(), old); err != nil {
			fmt.Fprintln(stderr, "hostel: warning:", err)
		}
	}
	fmt.Fprintf(stdout, "Server:  %s (%s)\n", resp.ServerID, g.URL())
	if g.Key != "" {
		fmt.Fprintf(stdout, "Key:     %s (pinned)\n", g.Key)
	}
	fmt.Fprintf(stdout, "Connected as %s (%s) from %s.\n", resp.User, resp.Role, res.Computer)
	fmt.Fprintln(stdout, "Run \"hostel\" to open the Hostel app. Agents on this computer use this connection.")
	return 0
}

func disconnectCmd(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel disconnect", stderr)
	if _, err := cli.Parse(fs, args); err != nil {
		return 2
	}
	g, err := config.ReadGuest()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	if g.Token == "" {
		fmt.Fprintln(stdout, "This computer is not connected.")
		return 0
	}
	warning, err := connect.Disconnect(context.Background(), g)
	if warning != nil {
		fmt.Fprintln(stderr, "hostel: warning:", warning)
	}
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Disconnected from %s.\n", g.URL())
	return 0
}

func usersCmd(args []string, stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	fs := cli.NewFlagSet("hostel config users "+sub, stderr)
	role := fs.String("role", "", "member or admin")
	name := fs.String("name", "", "the user's name (default: the name that the user gives at connect)")
	computer := fs.String("computer", "", "with remove: remove only this computer of the user")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return 2
	}
	switch {
	case sub == "" && len(pos) == 0:
		var users []protocol.User
		if err := c.Do(ctx, "GET", "/v1/users", nil, &users); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tROLE\tCOMPUTERS\tSINCE")
		for _, u := range users {
			computers := strings.Join(u.Computers, ", ")
			if computers == "" {
				computers = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", u.Name, u.Role, computers, u.CreatedAt.Local().Format(time.DateOnly))
		}
		tw.Flush()
		return 0
	case (sub == "invite" || sub == "code") && len(pos) == 0:
		req := protocol.InviteRequest{Role: *role, Name: *name, Self: sub == "code"}
		var resp protocol.InviteResponse
		if err := c.Do(ctx, "POST", "/v1/invites", req, &resp); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		intro := "Send this to the new user. They run it on their computer:"
		if sub == "code" {
			intro = "Run this on your other computer:"
		}
		printInvite(stdout, intro, resp.Code, resp.Fingerprint)
		return 0
	case sub == "set" && len(pos) == 1 && *role != "":
		if err := c.Do(ctx, "PATCH", "/v1/users/"+url.PathEscape(pos[0]), protocol.SetUserRequest{Role: *role}, nil); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s is now %s.\n", pos[0], *role)
		return 0
	case sub == "remove" && len(pos) == 1 && *computer != "":
		if err := c.Do(ctx, "DELETE", "/v1/users/"+url.PathEscape(pos[0])+"/computers/"+url.PathEscape(*computer), nil, nil); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		fmt.Fprintf(stdout, "Removed the computer %s of %s: it is disconnected.\n", *computer, pos[0])
		return 0
	case sub == "remove" && len(pos) == 1:
		if err := c.Do(ctx, "DELETE", "/v1/users/"+url.PathEscape(pos[0]), nil, nil); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		fmt.Fprintf(stdout, "Removed %s: their computers are disconnected and their leases end.\n", pos[0])
		return 0
	}
	fmt.Fprintln(stderr, `hostel: usage:
  hostel config users                              list the users
  hostel config users invite [--role member|admin] [--name "Name"]
  hostel config users code                         a code for your other computer
  hostel config users set <name> --role member|admin
  hostel config users remove <name> [--computer <computer>]`)
	return 2
}
