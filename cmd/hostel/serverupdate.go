package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/client"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/guest"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/update"
)

const serverUpdateUsage = `Usage: hostel server update [--version vX.Y.Z] [--config server.toml]
       hostel server update --remote [--version vX.Y.Z]

On the server machine: replaces the server binary and the room binaries in
helper_dir with the latest release (or --version), after a check of the
release signature; then restart the server (sudo systemctl restart hostel).
With --remote, on an admin's computer: the server updates itself and starts
again with the new binary; the command waits until it runs the new version.`

// remoteUpdateWait bounds the wait for the server's new version.
const remoteUpdateWait = 25 * time.Minute

func serverUpdate(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel server update", stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, serverUpdateUsage) }
	path := serverConfigFlag(fs)
	want := fs.String("version", "", "the release to install (default: the latest)")
	remote := fs.Bool("remote", false, "update the connected server from this computer (admin)")
	if rest, err := cli.Parse(fs, args); err != nil || len(rest) > 0 {
		fmt.Fprintln(stderr, serverUpdateUsage)
		return 2
	}
	var err error
	if *remote {
		err = remoteServerUpdate(*want, stdout)
	} else {
		err = localServerUpdate(*path, *want, stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, "hostel server update:", err)
		if errors.Is(err, update.ErrUnreachable) {
			return guest.ExitUnavailable
		}
		return 1
	}
	return 0
}

func localServerUpdate(path, want string, stdout io.Writer) error {
	file, err := resolveServerConfig(path)
	if err != nil {
		return err
	}
	paths, err := config.ReadPaths(file)
	if err != nil {
		return err
	}
	// The binary of the service (from its last start), not the binary that
	// runs this command: they can differ (/opt/hostel/bin and a copy on the
	// PATH).
	exe, err := update.Executable()
	if err != nil {
		return err
	}
	var addrs protocol.Addresses
	if data, err := os.ReadFile(filepath.Join(paths.DataDir, "server.json")); err == nil && json.Unmarshal(data, &addrs) == nil && addrs.Executable != "" {
		exe = addrs.Executable
	} else {
		fmt.Fprintf(stdout, "the server has not started yet (%s/server.json): this command updates %s\n", paths.DataDir, exe)
	}
	ctx := context.Background()
	have := binaryVersion(ctx, exe)
	if have == "" {
		return fmt.Errorf("the server binary %s does not run", exe)
	}
	src, err := update.Default()
	if err != nil {
		return err
	}
	tag := want
	if tag == "" {
		if tag, err = src.Latest(ctx); err != nil {
			return err
		}
	}
	if _, known := update.Parse(have); known && !update.Newer(tag, have) {
		fmt.Fprintf(stdout, "the server binary %s has %s: up to date (the release is %s)\n", exe, have, tag)
		return nil
	}
	r, err := src.Release(ctx, tag)
	if err != nil {
		return err
	}
	if err := update.Server(ctx, src, r, exe, paths.HelperDir); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "updated %s and %s from %s to %s\nrestart the server now, for example: sudo systemctl restart hostel\n", exe, paths.HelperDir, have, tag)
	return nil
}

func remoteServerUpdate(want string, stdout io.Writer) error {
	g, err := config.LoadGuest()
	if err != nil {
		return err
	}
	c, err := client.New(g)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteUpdateWait)
	defer cancel()
	var st protocol.ServerUpdate
	reqCtx, reqCancel := context.WithTimeout(ctx, time.Minute)
	err = c.Do(reqCtx, http.MethodPost, "/v1/server/update", protocol.ServerUpdateRequest{Version: want}, &st)
	reqCancel()
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "the server updates from %s to %s …\n", st.From, st.To)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("the server does not run %s after %s; see its log", st.To, remoteUpdateWait)
		case <-time.After(2 * time.Second):
		}
		reqCtx, reqCancel := context.WithTimeout(ctx, 10*time.Second)
		var info protocol.Info
		infoErr := c.Do(reqCtx, http.MethodGet, "/v1/info", nil, &info)
		var cur protocol.ServerUpdate
		stateErr := c.Do(reqCtx, http.MethodGet, "/v1/server/update", nil, &cur)
		reqCancel()
		switch {
		case infoErr == nil && info.Version == st.To:
			fmt.Fprintf(stdout, "the server runs %s\n", st.To)
			return nil
		case stateErr == nil && cur.State == protocol.UpdateFailed:
			return fmt.Errorf("the server did not update: %s", cur.Error)
		}
		// While the server starts again, the requests fail.
	}
}
