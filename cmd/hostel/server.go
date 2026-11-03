package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/connect"
	"github.com/csansoon/hostel/internal/server"
	"github.com/csansoon/hostel/internal/store"
	"github.com/csansoon/hostel/internal/tlspin"
)

func serverCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) >= 1 {
		switch args[0] {
		case "init":
			return serverInit(args[1:], os.Stdin, stdout, stderr)
		case "invite":
			return serverInvite(args[1:], stdout, stderr)
		case "run":
			return serverRun(args[1:], stderr)
		}
	}
	fmt.Fprintln(stderr, "hostel: usage: hostel server init|invite|run [--config server.toml]")
	return 2
}

// initOptions are the answers of "hostel server init" (flags, or questions
// in a terminal).
type initOptions struct {
	serverID string
	listen   string
	tls      string
	urls     []string
	user     string
}

func serverInit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel server init", stderr)
	path := serverConfigFlag(fs)
	var o initOptions
	var urls cli.StringList
	fs.StringVar(&o.serverID, "server-id", "", "the server's name, [a-z0-9]{1,20} (default: from the host name)")
	fs.StringVar(&o.listen, "listen", "", "the API address (default 127.0.0.1:7410)")
	fs.StringVar(&o.tls, "tls", config.TLSSelfSigned, "self-signed (own certificate) or off (behind a proxy with TLS)")
	fs.Var(&urls, "url", "an address that clients use, for example https://100.80.1.2:7410 (more than once for several)")
	fs.StringVar(&o.user, "user", "", "your name, for the first admin")
	if _, err := cli.Parse(fs, args); err != nil {
		return 2
	}
	o.urls = urls
	if isTerminal(stdin) {
		ask(stdin, stdout, &o)
	}
	if err := initServer(*path, o, stdout); err != nil {
		fmt.Fprintln(stderr, "hostel: server init:", err)
		return 1
	}
	return 0
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// ask asks for the values that the flags did not give.
func ask(stdin io.Reader, stdout io.Writer, o *initOptions) {
	in := bufio.NewScanner(stdin)
	line := func(prompt, def string) string {
		if def != "" {
			prompt += " [" + def + "]"
		}
		fmt.Fprintf(stdout, "  %s: ", prompt)
		if !in.Scan() {
			return def
		}
		if v := strings.TrimSpace(in.Text()); v != "" {
			return v
		}
		return def
	}
	fmt.Fprintln(stdout, "Hostel server setup")
	fmt.Fprintln(stdout)
	if o.serverID == "" {
		o.serverID = line("Name of this server", config.DefaultServerID())
	}
	if len(o.urls) == 0 {
		fmt.Fprintln(stdout, "  Addresses that clients use, one per line, empty line to end")
		fmt.Fprintln(stdout, "  (for example https://hostel.example.com or https://100.80.1.2:7410):")
		for {
			fmt.Fprint(stdout, "  > ")
			if !in.Scan() || strings.TrimSpace(in.Text()) == "" {
				break
			}
			o.urls = append(o.urls, strings.TrimSpace(in.Text()))
		}
	}
	if o.user == "" {
		o.user = line("Your name", connect.GitUserName())
	}
	fmt.Fprintln(stdout)
}

func initServer(path string, o initOptions, stdout io.Writer) error {
	file, err := resolveServerConfig(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
		if o.serverID == "" {
			o.serverID = config.DefaultServerID()
		}
		if o.user = strings.TrimSpace(o.user); o.user != "" {
			if p := server.NameProblem(o.user); p != "" {
				return fmt.Errorf("--user: %s", p)
			}
		}
		if !config.ValidServerID(o.serverID) {
			return fmt.Errorf("the server name %q must be 1 to 20 lowercase letters and digits, for example %q", o.serverID, config.DefaultServerID())
		}
		if o.listen == "" {
			o.listen = "127.0.0.1:7410"
		}
		if _, _, err := net.SplitHostPort(o.listen); err != nil {
			return fmt.Errorf("--listen: %q is not a host:port address, for example 0.0.0.0:7410", o.listen)
		}
		if p := config.TLSProblem(o.tls); p != "" {
			return fmt.Errorf("--tls: %s", p)
		}
		if len(o.urls) == 0 {
			scheme := "https"
			if o.tls == config.TLSOff {
				scheme = "http"
			}
			u := scheme + "://" + o.listen
			if config.PublicURLProblem(u, o.tls) != "" {
				return fmt.Errorf("--url: give the address that clients use, for example --url https://hostel.example.com (the proxy with TLS)")
			}
			o.urls = []string{u}
		}
		for _, u := range o.urls {
			if p := config.PublicURLProblem(u, o.tls); p != "" {
				return fmt.Errorf("--url: %s", p)
			}
		}
		if err := os.WriteFile(file, []byte(config.Template(o.serverID, o.listen, o.tls, o.urls)), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote %s\n", file)
	} else if err != nil {
		return err
	}
	schema, err := config.ServerSchema()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(file), "hostel.schema.json"), append(schema, '\n'), 0o644); err != nil {
		return err
	}
	paths, err := config.ReadPaths(file)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		return err
	}
	// "hostel admin config check" of a server.toml from before M9 reads it.
	if err := os.MkdirAll(paths.PlatformsDir, 0o755); err != nil {
		return err
	}
	for _, k := range []struct{ file, comment string }{
		{server.GatewayHostKeyFile, "hostel-gateway"},
		{server.ServerKeyFile, "hostel-server"},
	} {
		created, err := server.EnsureKey(filepath.Join(paths.DataDir, k.file), k.comment)
		if err != nil {
			return fmt.Errorf("create %s: %w", k.file, err)
		}
		if created {
			fmt.Fprintf(stdout, "created %s\n", filepath.Join(paths.DataDir, k.file))
		}
	}
	if paths.TLS == config.TLSSelfSigned {
		if err := tlspin.EnsureCert(paths.DataDir); err != nil {
			return err
		}
	}
	pub, err := os.ReadFile(filepath.Join(paths.DataDir, server.ServerKeyFile+".pub"))
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "server public key (add it to authorized_keys on static machines):\n%s", pub)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	st, err := store.Open(ctx, filepath.Join(paths.DataDir, "hostel.db"))
	if err != nil {
		return err
	}
	users, err := st.Users(ctx)
	st.Close()
	if err != nil {
		return err
	}
	if len(users) > 0 {
		fmt.Fprintln(stdout, "the server has users already; \"hostel server invite\" makes a new admin invite")
		return nil
	}
	code, fingerprint, err := server.LocalInvite(ctx, paths, store.Invite{Kind: store.InviteUser, Role: store.RoleAdmin, Name: o.user})
	if err != nil {
		fmt.Fprintf(stdout, "no admin invite yet: %v\nAfter that, run \"hostel server invite\".\n", err)
		return nil
	}
	printInvite(stdout, "Connect your computer as the first admin. Run this on your computer:", code, fingerprint)
	return nil
}

func printInvite(w io.Writer, intro, code, fingerprint string) {
	fmt.Fprintf(w, "\n%s\n\n  hostel connect %s\n\n", intro, code)
	if fingerprint != "" {
		fmt.Fprintf(w, "Server key: %s\n", fingerprint)
	}
	fmt.Fprintln(w, "The code works once, for 24 hours.")
}

// serverInvite is "hostel server invite" on the server machine: an invite
// without the API (for example when nobody can sign in as admin).
func serverInvite(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel server invite", stderr)
	path := serverConfigFlag(fs)
	role := fs.String("role", store.RoleAdmin, "admin or member")
	name := fs.String("name", "", "the user's name (default: the name that the user gives at connect)")
	if _, err := cli.Parse(fs, args); err != nil {
		return 2
	}
	if *role != store.RoleAdmin && *role != store.RoleMember {
		fmt.Fprintln(stderr, "hostel: --role must be admin or member")
		return 2
	}
	file, err := resolveServerConfig(*path)
	if err == nil {
		var paths config.Paths
		if paths, err = config.ReadPaths(file); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var code, fingerprint string
			code, fingerprint, err = server.LocalInvite(ctx, paths, store.Invite{Kind: store.InviteUser, Role: *role, Name: *name})
			if err == nil {
				printInvite(stdout, "Run this on the user's computer:", code, fingerprint)
				return 0
			}
		}
	}
	fmt.Fprintln(stderr, "hostel: server invite:", err)
	return 1
}

func serverRun(args []string, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel server run", stderr)
	path := serverConfigFlag(fs)
	if _, err := cli.Parse(fs, args); err != nil {
		return 2
	}
	file, err := resolveServerConfig(*path)
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	if err := server.Run(ctx, file, log); errors.Is(err, server.ErrConfig) {
		fmt.Fprintln(stderr, "hostel: the server does not start:", err)
		return 1
	} else if err != nil {
		fmt.Fprintln(stderr, "hostel: server:", err)
		return 1
	}
	return 0
}
