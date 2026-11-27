package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/protocol"
)

const secretsUsage = `hostel: usage:
  hostel config secrets                         the secrets: name, size, when set, the tools that use them (never the values)
  hostel config secrets set <name> <file|->     set a secret from a file, or from stdin with - (a one-line
                                                text loses its final line break; other values stay as they are)
  hostel config secrets remove <name>           remove a secret (no tool may use it)`

// maxSecretFile matches the server's limit (server.MaxSecretSize).
const maxSecretFile = 64 << 10

// secretsCmd is "hostel config secrets" (admin, M14): files that the server
// gives to the start and stop scripts of the tools that declare them.
func secretsCmd(args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	fs := cli.NewFlagSet("hostel config secrets "+sub, stderr)
	pos, err := cli.Parse(fs, args)
	if err != nil {
		return 2
	}
	switch {
	case sub == "" && len(pos) == 0:
		return secretsList(stdout, stderr)
	case sub == "set" && len(pos) == 2:
		return secretSet(pos[0], pos[1], stdout, stderr)
	case sub == "remove" && len(pos) == 1:
		return secretRequest("DELETE", pos[0], nil, fmt.Sprintf("Removed the secret %s.", pos[0]), stdout, stderr)
	}
	fmt.Fprintln(stderr, secretsUsage)
	return 2
}

func secretsList(stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var list []protocol.Secret
	if err := c.Do(ctx, "GET", "/v1/secrets", nil, &list); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "No secrets. A tool declares the secrets that it needs (secrets in tool.toml); set one with \"hostel config secrets set <name> <file>\".")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SECRET\tSIZE\tSET\tTOOLS")
	for _, sec := range list {
		size, set := "-", "not set"
		if sec.Set {
			size, set = fmt.Sprintf("%d B", sec.Size), sec.SetAt.Local().Format("2006-01-02 15:04")
		}
		tools := strings.Join(sec.Tools, ", ")
		if tools == "" {
			tools = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", sec.Name, size, set, tools)
	}
	tw.Flush()
	return 0
}

func secretSet(name, file string, stdout, stderr io.Writer) int {
	var r io.Reader = os.Stdin
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		defer f.Close()
		r = f
	}
	value, err := io.ReadAll(io.LimitReader(r, maxSecretFile+1))
	if err != nil {
		fmt.Fprintln(stderr, "hostel: read the secret:", err)
		return 1
	}
	if file == "-" && oneLineText(value) {
		// A password or an ID from echo or a terminal ends with a line break
		// that is not part of it. Other values (a .p12) stay as they are.
		value = []byte(strings.TrimRight(string(value), "\r\n"))
	}
	switch {
	case len(value) == 0:
		fmt.Fprintln(stderr, "hostel: the secret is empty")
		return 2
	case len(value) > maxSecretFile:
		fmt.Fprintf(stderr, "hostel: the secret is larger than %d bytes\n", maxSecretFile)
		return 2
	}
	return secretRequest("PUT", name, protocol.SetSecret{Value: value},
		fmt.Sprintf("Stored the secret %s (%d B) on the server. Leases get it when they start a tool that uses it.", name, len(value)), stdout, stderr)
}

// oneLineText reports a value that is valid UTF-8 text with no line break or
// control character before its final line breaks.
func oneLineText(v []byte) bool {
	s := strings.TrimRight(string(v), "\r\n")
	if !utf8.ValidString(s) {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 && r != '\t' || r == 0x7f })
}

func secretRequest(method, name string, body any, done string, stdout, stderr io.Writer) int {
	c, err := apiClient()
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Do(ctx, method, "/v1/secrets/"+url.PathEscape(name), body, nil); err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	fmt.Fprintln(stdout, done)
	return 0
}
