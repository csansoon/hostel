package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/server"
	"github.com/csansoon/hostel/internal/store"
)

func serverCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) >= 1 {
		switch args[0] {
		case "init":
			return serverInit(args[1:], stdout, stderr)
		case "run":
			return serverRun(args[1:], stderr)
		}
	}
	fmt.Fprintln(stderr, "hostel: usage: hostel server init|run [--config server.toml]")
	return 2
}

func serverInit(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel server init", stderr)
	path := serverConfigFlag(fs)
	if _, err := cli.Parse(fs, args); err != nil {
		return 2
	}
	if err := initServer(*path, stdout); err != nil {
		fmt.Fprintln(stderr, "hostel: server init:", err)
		return 1
	}
	return 0
}

func initServer(path string, stdout io.Writer) error {
	file, err := resolveServerConfig(path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(file, []byte(config.Template(config.DefaultServerID())), 0o644); err != nil {
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
	defer st.Close()
	n, err := st.CountTokens(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		fmt.Fprintln(stdout, "the database already has tokens; no new admin token")
		return nil
	}
	_, secret, err := st.CreateToken(ctx, "admin", protocol.RoleAdmin, "", time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "admin token (shown only once): %s\n", secret)

	guestFile, err := config.GuestPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(guestFile); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(guestFile), 0o755); err != nil {
			return err
		}
		body := fmt.Sprintf("server = %q\ntoken = %q\n", "http://"+paths.Listen, secret)
		if err := os.WriteFile(guestFile, []byte(body), 0o600); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote %s with the admin token\n", guestFile)
	}
	return nil
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
	cfg, err := config.LoadServer(file)
	if err != nil {
		fmt.Fprintln(stderr, "hostel: the server does not start: configuration errors:")
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	if err := server.Run(ctx, cfg, log); err != nil {
		fmt.Fprintln(stderr, "hostel: server:", err)
		return 1
	}
	return 0
}
