package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
	"github.com/pelletier/go-toml/v2"
)

func adminCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) >= 1 && args[0] == "config" {
		if len(args) >= 2 {
			switch args[1] {
			case "check":
				return adminConfigCheck(args[2:], stdout, stderr)
			case "schema":
				return adminConfigSchema(args[2:], stdout, stderr)
			}
		}
		fmt.Fprintln(stderr, "hostel: usage: hostel admin config check|schema [--config server.toml]")
		return 2
	}
	return notImplemented("admin", stderr)
}

func serverConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", "", "server configuration file (default ~/.config/hostel/server.toml)")
}

func resolveServerConfig(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	return config.DefaultServerPath()
}

func adminConfigCheck(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel admin config check", stderr)
	path := serverConfigFlag(fs)
	repo := fs.String("repo", "", "check a repository file (hostel.toml) instead of the server configuration")
	if _, err := cli.Parse(fs, args); err != nil {
		return 2
	}
	if *repo != "" {
		if _, err := config.LoadRepo(*repo); err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s: ok\n", *repo)
		return 0
	}
	file, err := resolveServerConfig(*path)
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	cfg, err := config.LoadServer(file)
	if err != nil {
		fmt.Fprintln(stderr, "hostel:", err)
		return 1
	}
	out, err := toml.Marshal(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "hostel: print the configuration:", err)
		return 1
	}
	fmt.Fprintf(stdout, "# %s: ok. Effective configuration with all defaults:\n", file)
	stdout.Write(out)
	for _, name := range config.PlatformNames(cfg.Platforms) {
		p := cfg.Platforms[name]
		out, err := toml.Marshal(p)
		if err != nil {
			fmt.Fprintln(stderr, "hostel: print the configuration:", err)
			return 1
		}
		dir := p.Dir
		if dir == "" {
			dir = "(no directory)"
		}
		fmt.Fprintf(stdout, "\n# platform %s (%s): %s\n", name, p.Kind, dir)
		stdout.Write(out)
		for _, osName := range config.OSNames {
			for _, step := range []string{config.StepProvision, config.StepPrepare, config.StepScreenshot, config.StepReset, config.StepHealth} {
				if s := p.Script(osName, step); s != "" {
					fmt.Fprintf(stdout, "# script %s %s: %s (timeout %s)\n", osName, step, s, p.ScriptTimeout(step))
				}
			}
		}
	}
	return 0
}

func adminConfigSchema(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel admin config schema", stderr)
	path := serverConfigFlag(fs)
	out := fs.String("o", "", "output file (default hostel.schema.json next to the server configuration)")
	if _, err := cli.Parse(fs, args); err != nil {
		return 2
	}
	if *out == "" {
		file, err := resolveServerConfig(*path)
		if err != nil {
			fmt.Fprintln(stderr, "hostel:", err)
			return 1
		}
		*out = filepath.Join(filepath.Dir(file), "hostel.schema.json")
	}
	data, err := config.ServerSchema()
	if err != nil {
		fmt.Fprintln(stderr, "hostel: make the schema:", err)
		return 1
	}
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintln(stderr, "hostel: write the schema:", err)
		return 1
	}
	fmt.Fprintln(stdout, *out)
	return 0
}
