package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

const configUsage = `hostel: usage:
  hostel config                          the configuration document (providers, machines, settings)
  hostel config edit                     change it in $EDITOR
  hostel config apply <file|->           replace it with a file
  hostel config history                  the revisions
  hostel config settings [set <key>=<value> ...]
  hostel config rooms [set|remove|drain|undrain|repair]
  hostel config providers [add|remove]
  hostel config tools [new|pull|push|reset|remove]
  hostel config users [invite|code|set|remove]`

// configCmd is "hostel config": the server configuration (multiplayer.md,
// section 7). Members read it; admins change it.
func configCmd(args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "users":
		return usersCmd(args, stdout, stderr)
	case "tools":
		return toolsCmd(args, stdout, stderr)
	case "rooms":
		return roomsCmd(args, stdout, stderr)
	case "providers":
		return providersCmd(args, stdout, stderr)
	case "":
		return configShow(stdout, stderr)
	case "edit":
		return configEdit(args, stdout, stderr)
	case "apply":
		return configApply(args, stdout, stderr)
	case "history":
		return configHistory(stdout, stderr)
	case "settings":
		return configSettings(args, stdout, stderr)
	}
	fmt.Fprintln(stderr, configUsage)
	return 2
}

func getConfig(c *client.Client) (protocol.ConfigDocument, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var doc protocol.ConfigDocument
	err := c.Do(ctx, "GET", "/v1/config", nil, &doc)
	return doc, err
}

func putConfig(c *client.Client, base int, document string) (int, error) {
	// A change can check a provider login or run an external provider's
	// capabilities command on the server.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	var doc protocol.ConfigDocument
	err := c.Do(ctx, "PUT", "/v1/config", protocol.PutConfigRequest{BaseRevision: base, Document: document}, &doc)
	return doc.Revision, err
}

func configShow(stdout, stderr io.Writer) int {
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
	fmt.Fprintf(stdout, "# Revision %d of the configuration of %s.\n", doc.Revision, c.Base())
	if doc.Hidden {
		fmt.Fprintln(stdout, "# Secret values (env, command_env, github_token) are hidden: only admins see them.")
	}
	if doc.Document == "" {
		fmt.Fprintln(stdout, "# Empty: add a machine with \"hostel config edit\".")
	}
	fmt.Fprint(stdout, doc.Document)
	return 0
}

func configApply(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel config apply", stderr)
	pos, err := cli.Parse(fs, args)
	if err != nil || len(pos) != 1 {
		fmt.Fprintln(stderr, "hostel: usage: hostel config apply <file|->")
		return 2
	}
	var data []byte
	if pos[0] == "-" {
		data, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	} else {
		data, err = os.ReadFile(pos[0])
	}
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
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
	rev, err := putConfig(c, doc.Revision, string(data))
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Revision %d applies now.\n", rev)
	return 0
}

// changeConfig applies edit to the latest document and uploads it.
func changeConfig(stdout, stderr io.Writer, edit func(doc []byte) ([]byte, error)) int {
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
	if doc.Hidden {
		fmt.Fprintln(stderr, "hostel: only an admin can change the configuration; ask an admin of the server")
		return 1
	}
	out, err := edit([]byte(doc.Document))
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	rev, err := putConfig(c, doc.Revision, string(out))
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Revision %d applies now.\n", rev)
	return 0
}

func configEdit(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel config edit", stderr)
	if _, err := cli.Parse(fs, args); err != nil {
		return 2
	}
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
	if doc.Hidden {
		fmt.Fprintln(stderr, "hostel: only an admin can change the configuration; ask an admin of the server")
		return 1
	}
	f, err := os.CreateTemp("", "hostel-config-*.toml")
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	file := f.Name()
	f.Close()
	text := doc.Document
	in := bufio.NewScanner(os.Stdin)
	for {
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		if err := runEditor(file); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		data, err := os.ReadFile(file)
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		if string(data) == doc.Document {
			os.Remove(file)
			fmt.Fprintln(stdout, "No change.")
			return 0
		}
		rev, err := putConfig(c, doc.Revision, string(data))
		if err == nil {
			os.Remove(file)
			fmt.Fprintf(stdout, "Revision %d applies now.\n", rev)
			return 0
		}
		fmt.Fprintln(stderr, "hostel:", err)
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.Code == protocol.CodeConflict || !isTerminal(os.Stdin) {
			fmt.Fprintf(stderr, "hostel: your changes are in %s\n", file)
			return 1
		}
		fmt.Fprint(stdout, "Edit again? [Y/n] ")
		if !in.Scan() || strings.EqualFold(strings.TrimSpace(in.Text()), "n") {
			fmt.Fprintf(stderr, "hostel: your changes are in %s\n", file)
			return 1
		}
		text = string(data)
	}
}

func runEditor(file string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
		if runtime.GOOS == "windows" {
			editor = "notepad"
		}
	}
	// A person edits; the deadline only ends a forgotten editor.
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	defer cancel()
	parts := strings.Fields(editor)
	cmd := exec.CommandContext(ctx, parts[0], append(parts[1:], file)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor %s: %w", editor, err)
	}
	return nil
}

func configHistory(stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var revs []protocol.ConfigRevision
	if err := c.Do(ctx, "GET", "/v1/config/history", nil, &revs); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "REVISION\tUSER\tTIME")
	for _, r := range revs {
		fmt.Fprintf(tw, "%d\t%s\t%s\n", r.Revision, r.User, r.CreatedAt.Local().Format(time.DateTime))
	}
	tw.Flush()
	return 0
}

func configSettings(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
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
		settings, _, err := config.RemoveTable([]byte(doc.Document), []string{"providers"})
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		if strings.TrimSpace(string(settings)) == "" {
			fmt.Fprintln(stdout, "# All settings have their default values (docs/design.md, section 9).")
		}
		fmt.Fprint(stdout, string(settings))
		return 0
	}
	if args[0] != "set" || len(args) < 2 {
		fmt.Fprintln(stderr, "hostel: usage: hostel config settings [set <key>=<value> ...], for example: hostel config settings set idle_timeout=20m")
		return 2
	}
	type kv struct {
		path  []string
		value string
	}
	var sets []kv
	for _, a := range args[1:] {
		k, v, ok := strings.Cut(a, "=")
		path := strings.Split(strings.TrimSpace(k), ".")
		if !ok || !config.IsDocumentKey(path[0]) || path[0] == "providers" {
			fmt.Fprintf(stderr, "hostel: %q is not a setting; settings are idle_timeout, poll_timeout, limit_retry, port_process_offline, intervals.*, and mise.*\n", a)
			return 2
		}
		sets = append(sets, kv{path, tomlValue(strings.TrimSpace(v))})
	}
	return changeConfig(stdout, stderr, func(doc []byte) ([]byte, error) {
		var err error
		for _, s := range sets {
			if doc, err = config.SetKey(doc, s.path, s.value); err != nil {
				return nil, err
			}
		}
		return doc, nil
	})
}

// tomlValue returns v as TOML: as written if it is a TOML value (a number, a
// boolean, an array), or else as a string.
func tomlValue(v string) string {
	var probe map[string]any
	if toml.Unmarshal([]byte("v = "+v), &probe) == nil {
		return v
	}
	return fmt.Sprintf("%q", v)
}
