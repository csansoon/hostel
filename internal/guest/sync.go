package guest

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/syncfs"
)

// syncPaths lists the files to send: the files that git does not ignore, plus
// the files that match the include patterns of hostel.toml.
func syncPaths(repo *Repo, rc *config.Repo) (paths, extra []string, err error) {
	out, err := gitOutput(repo.Root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, nil, err
	}
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	for _, pattern := range rc.Include {
		matches, err := filepath.Glob(filepath.Join(repo.Root, filepath.FromSlash(pattern)))
		if err != nil {
			return nil, nil, fmt.Errorf("include pattern %q: %w", pattern, err)
		}
		for _, m := range matches {
			if info, err := os.Lstat(m); err != nil || info.IsDir() {
				continue
			}
			rel, err := filepath.Rel(repo.Root, m)
			if err != nil {
				return nil, nil, err
			}
			rel = filepath.ToSlash(rel)
			if !slices.Contains(paths, rel) && !slices.Contains(extra, rel) {
				extra = append(extra, rel)
			}
		}
	}
	return append(paths, extra...), extra, nil
}

// syncFiles sends the changed files to the room (design, section 12) and
// returns the paths it sent.
func syncFiles(ctx context.Context, st *State, repo *Repo, rc *config.Repo, stderr io.Writer) ([]string, error) {
	paths, extra, err := syncPaths(repo, rc)
	if err != nil {
		return nil, err
	}
	if len(extra) > 0 {
		fmt.Fprintf(stderr, "hostel: sending extra files from include: %s\n", strings.Join(extra, ", "))
	}
	manifest, err := syncfs.BuildManifest(repo.Root, paths)
	if err != nil {
		return nil, err
	}
	conn, err := Dial(ctx, st, "cmd")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var plan syncfs.Plan
	if err := conn.Helper(ctx, []string{"sync-plan"}, manifest, &plan); err != nil {
		return nil, fmt.Errorf("sync plan: %w", err)
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(syncfs.WriteTar(pw, repo.Root, manifest, plan.Send)) }()
	var res struct {
		Written int `json:"written"`
		Deleted int `json:"deleted"`
	}
	err = conn.Helper(ctx, []string{"sync-apply", plan.PlanID}, pr, &res)
	pr.Close()
	if err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	fmt.Fprintf(stderr, "hostel: sync: %d files sent, %d deleted (%d files in the work directory)\n", res.Written, res.Deleted, len(manifest.Entries))
	if len(plan.Overwrite) > 0 {
		fmt.Fprintf(stderr, "hostel: these room changes were replaced by the local files: %s\n", strings.Join(plan.Overwrite, ", "))
	}
	return plan.Send, nil
}

// Sync is "hostel sync [--setup]".
func Sync(args []string, stdout, stderr io.Writer) int {
	fs := cli.NewFlagSet("hostel sync", stderr)
	platform := fs.String("platform", "", "lease platform, if the worktree has several leases")
	setup := fs.Bool("setup", false, "run the repository setup job again")
	wait := fs.Duration("wait", 100*time.Second, "time to wait for the setup job before returning")
	if _, err := cli.Parse(fs, args); err != nil {
		return ExitUsage
	}
	l, code := openLease(*platform, stderr)
	if l == nil {
		return code
	}
	deadline := time.Now().Add(*wait)
	ctx := context.Background()
	rc, err := l.repo.RepoConfig()
	if err != nil {
		return fail(stderr, err)
	}
	// A running setup job from an earlier command continues; no new sync then.
	cmd := ""
	if l.st.SetupJob == "" {
		sent, err := syncFiles(ctx, l.st, l.repo, rc, stderr)
		if err != nil {
			return fail(stderr, err)
		}
		switch {
		case *setup:
			cmd = setupCommand(rc, l.st.Platform, true)
		case rc.MiseEnabled() && slices.ContainsFunc(sent, func(p string) bool { return slices.Contains(miseFiles, p) }):
			cmd = setupCommand(rc, l.st.Platform, false)
		}
	}
	code, err = runSetup(ctx, l.st, cmd, deadline, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	if code == ExitNotFinished {
		fmt.Fprintln(stderr, "hostel: not finished; run \"hostel sync\" again")
	}
	return code
}
