package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

const roomsUsage = `hostel: usage:
  hostel config rooms                                the machines and their rooms
  hostel config rooms add <name> [--tool t] [--provider p]
                                                     a code for "hostel machine join" on the new machine
  hostel config rooms set <machine> <key>=<value> ...  for example tools=xcode,ios-simulator,jdk:21
  hostel config rooms remove <machine>
  hostel config rooms drain|undrain <machine|room>     no new leases on the room / new leases again
  hostel config rooms repair <machine|room> [--accept-host-key]
  hostel config rooms destroy <room>                  an on-demand room without a lease
A machine is <provider>/<machine>, or only <machine> when the name is unique.`

func roomsCmd(args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	fs := cli.NewFlagSet("hostel config rooms "+sub, stderr)
	accept := fs.Bool("accept-host-key", false, "with repair: accept a changed host key of the room")
	provider := fs.String("provider", "", "with add: the static provider (default home)")
	var tools cli.StringList
	fs.Var(&tools, "tool", "with add: a tool of the machine, name or name:option (more than once for several)")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return 2
	}
	switch {
	case sub == "" && len(pos) == 0:
		return roomsList(stdout, stderr)
	case sub == "add" && len(pos) == 1:
		return roomsAdd(protocol.MachineInviteRequest{Name: pos[0], Provider: *provider, Tools: tools}, stdout, stderr)
	case sub == "set" && len(pos) >= 2:
		return roomsSet(pos[0], pos[1:], stdout, stderr)
	case sub == "remove" && len(pos) == 1:
		return roomsRemove(pos[0], stdout, stderr)
	case (sub == "drain" || sub == "undrain" || sub == "repair" || sub == "destroy") && len(pos) == 1:
		return roomAction(sub, pos[0], *accept, stdout, stderr)
	}
	fmt.Fprintln(stderr, roomsUsage)
	return 2
}

func getStatus(c *client.Client) (protocol.Status, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var st protocol.Status
	err := c.Do(ctx, "GET", "/v1/status", nil, &st)
	return st, err
}

func roomsList(stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	doc, err := getConfig(c)
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	st, err := getStatus(c)
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	providers, err := config.DocumentProviders([]byte(doc.Document))
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MACHINE\tKIND\tOS\tTOOLS\tROOMS")
	for _, p := range providers {
		for _, m := range p.Machines {
			var rooms []string
			for _, r := range st.Rooms {
				if r.Provider == m.Provider && r.Machine == m.Name {
					state := r.State
					if r.Drained {
						state += ", drained"
					}
					rooms = append(rooms, fmt.Sprintf("%s (%s)", r.Name, state))
				}
			}
			osName := m.OS
			if osName == "" {
				osName = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.Key(), p.Kind, osName, orDash(strings.Join(m.Tools, ",")),
				orDash(strings.Join(rooms, "; ")))
		}
	}
	tw.Flush()
	return 0
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// findMachine resolves "<provider>/<machine>" or a unique "<machine>".
func findMachine(doc []byte, name string) (config.DocMachine, error) {
	providers, err := config.DocumentProviders(doc)
	if err != nil {
		return config.DocMachine{}, err
	}
	var found []config.DocMachine
	for _, p := range providers {
		for _, m := range p.Machines {
			if m.Key() == name || m.Name == name {
				found = append(found, m)
			}
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return config.DocMachine{}, fmt.Errorf("no machine %q; \"hostel config rooms\" lists them", name)
	}
	return config.DocMachine{}, fmt.Errorf("several machines are named %q; use <provider>/<machine>", name)
}

// listKeys are machine keys whose value a user writes as a comma list.
var listKeys = map[string]bool{"tools": true}

func roomsSet(name string, sets []string, stdout, stderr io.Writer) int {
	return changeConfig(stdout, stderr, func(doc []byte) ([]byte, error) {
		m, err := findMachine(doc, name)
		if err != nil {
			return nil, err
		}
		for _, a := range sets {
			k, v, ok := strings.Cut(a, "=")
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if !ok || k == "" {
				return nil, fmt.Errorf("%q is not <key>=<value>, for example tools=xcode,ios-simulator", a)
			}
			value := tomlValue(v)
			if listKeys[k] {
				var items []string
				for _, item := range strings.Split(v, ",") {
					if item = strings.TrimSpace(item); item != "" {
						items = append(items, fmt.Sprintf("%q", item))
					}
				}
				value = "[" + strings.Join(items, ", ") + "]"
			}
			path := append([]string{"providers", m.Provider, "machines", m.Name}, strings.Split(k, ".")...)
			if doc, err = config.SetKey(doc, path, value); err != nil {
				return nil, err
			}
		}
		return doc, nil
	})
}

func roomsAdd(req protocol.MachineInviteRequest, stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var resp protocol.MachineInviteResponse
	if err := c.Do(ctx, "POST", "/v1/machines/invites", req, &resp); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	fmt.Fprintf(stdout, `Run this on the new machine as an administrator, before %s.

macOS or Linux:

  %s

Windows (PowerShell as an administrator):

  %s

It creates the user "hostel", installs Hostel, and starts the service that
connects the machine to the server. The server adds %s when it joins;
"hostel status" shows its room. The code works once.
`, resp.ExpiresAt.Local().Format("2006-01-02 15:04"), resp.Unix, resp.Windows, resp.Machine)
	return 0
}

func roomsRemove(name string, stdout, stderr io.Writer) int {
	return changeConfig(stdout, stderr, func(doc []byte) ([]byte, error) {
		m, err := findMachine(doc, name)
		if err != nil {
			return nil, err
		}
		out, ok, err := config.RemoveTable(doc, []string{"providers", m.Provider, "machines", m.Name})
		if err == nil && !ok {
			err = fmt.Errorf("the machine %s is not a table in the configuration; remove it with \"hostel config edit\"", m.Key())
		}
		return out, err
	})
}

func roomAction(action, name string, accept bool, stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	st, err := getStatus(c)
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	var found []protocol.RoomStatus
	for _, r := range st.Rooms {
		if r.Name == name || r.Provider+"/"+r.Machine == name || r.Machine == name {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		var names []string
		for _, r := range found {
			names = append(names, r.Name)
		}
		if len(found) == 0 {
			fmt.Fprintf(stderr, "hostel: no room of %q; \"hostel config rooms\" lists them\n", name)
		} else {
			fmt.Fprintf(stderr, "hostel: %q has several rooms (%s); give the room name\n", name, strings.Join(names, ", "))
		}
		return 1
	}
	var body any
	if action == "repair" {
		body = map[string]bool{"accept_host_key": accept}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Do(ctx, "POST", fmt.Sprintf("/v1/rooms/%d/%s", found[0].ID, url.PathEscape(action)), body, nil); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Room %s: %s requested.\n", found[0].Name, action)
	return 0
}

const providersUsage = `hostel: usage:
  hostel config providers                                       the providers
  hostel config providers add <name> --kind static|namespace|external
  hostel config providers remove <name>                         no on-demand rooms may be left
  hostel config providers login namespace [--cli devbox|nsc]    log the server in to Namespace`

func providersCmd(args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	fs := cli.NewFlagSet("hostel config providers "+sub, stderr)
	kind := fs.String("kind", "", "with add: static, namespace, or external")
	only := fs.String("cli", "", "with login: only this CLI (devbox or nsc)")
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return 2
	}
	switch {
	case sub == "" && len(pos) == 0:
		c, err := apiClient()
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		doc, err := getConfig(c)
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		providers, err := config.DocumentProviders([]byte(doc.Document))
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tKIND\tMACHINES")
		for _, p := range providers {
			var names []string
			for _, m := range p.Machines {
				names = append(names, m.Name)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", p.Name, p.Kind, orDash(strings.Join(names, ", ")))
		}
		tw.Flush()
		return 0
	case sub == "add" && len(pos) == 1 && *kind != "":
		name := pos[0]
		code := changeConfig(stdout, stderr, func(doc []byte) ([]byte, error) {
			providers, err := config.DocumentProviders(doc)
			if err != nil {
				return nil, err
			}
			for _, p := range providers {
				if p.Name == name {
					return nil, fmt.Errorf("the provider %s exists already", name)
				}
			}
			return config.SetKey(doc, []string{"providers", name, "kind"}, fmt.Sprintf("%q", *kind))
		})
		if code == 0 {
			fmt.Fprintf(stdout, "Added the provider %s. Add machines with \"hostel config edit\".\n", name)
		}
		return code
	case sub == "login" && len(pos) == 1 && pos[0] == "namespace":
		clis := []string{"devbox", "nsc"}
		if *only != "" {
			clis = []string{*only}
		}
		return providerLogin(clis, stdout, stderr)
	case sub == "remove" && len(pos) == 1:
		return changeConfig(stdout, stderr, func(doc []byte) ([]byte, error) {
			out, ok, err := config.RemoveTable(doc, []string{"providers", pos[0]})
			if err == nil && !ok {
				err = fmt.Errorf("no provider %q; \"hostel config providers\" lists them", pos[0])
			}
			return out, err
		})
	}
	fmt.Fprintln(stderr, providersUsage)
	return 2
}

// providerLogin logs the server's provider CLIs in: the server runs the
// login, and the admin opens the address on this computer.
func providerLogin(clis []string, stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	for _, cli := range clis {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		var l protocol.Login
		err := c.Do(ctx, "POST", "/v1/logins/"+cli, nil, &l)
		cancel()
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "not_installed" {
			fmt.Fprintf(stdout, "%s: not installed on the server; skipped.\n", cli)
			continue
		}
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		if l.Already {
			fmt.Fprintf(stdout, "%s: the server is logged in already.\n", cli)
			continue
		}
		if l.URL != "" {
			fmt.Fprintf(stdout, "%s: the server needs a Namespace login. Open this address and sign in:\n  %s\nWaiting...\n", cli, l.URL)
		}
		end := time.Now().Add(11 * time.Minute)
		for l.State == protocol.LoginWaiting && time.Now().Before(end) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			err := c.Do(ctx, "GET", "/v1/logins/"+cli+"?wait=1", nil, &l)
			cancel()
			if err != nil {
				fmt.Fprintln(stderr, "hostel:", err)
				return 1
			}
		}
		switch l.State {
		case protocol.LoginDone:
			fmt.Fprintf(stdout, "%s: signed in.\n", cli)
		case protocol.LoginFailed:
			fmt.Fprintf(stderr, "hostel: %s\n", l.Error)
			return 1
		default:
			fmt.Fprintf(stderr, "hostel: %s: the login did not end in 10 minutes; run the command again\n", cli)
			return 1
		}
	}
	return 0
}
