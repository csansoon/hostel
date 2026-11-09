package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/csansoon/hostel/internal/cli"
	"github.com/csansoon/hostel/internal/syncfs"
)

// gitTimeout bounds each git command of the baseline.
const gitTimeout = 2 * time.Minute

type storedPlan struct {
	Plan     syncfs.Plan     `json:"plan"`
	Manifest syncfs.Manifest `json:"manifest"`
}

func (h *helper) manifestPath(room string) string {
	return filepath.Join(h.roomDir(room), "manifest.json")
}

func (h *helper) syncPlan(args []string, stdin io.Reader) (syncfs.Plan, error) {
	room, _, err := roomFlag("sync-plan", args)
	if err != nil {
		return syncfs.Plan{}, err
	}
	if _, err := h.readLease(room); err != nil {
		return syncfs.Plan{}, err
	}
	var next syncfs.Manifest
	if err := json.NewDecoder(stdin).Decode(&next); err != nil {
		return syncfs.Plan{}, fmt.Errorf("read the manifest: %w", err)
	}
	var old syncfs.Manifest
	if data, err := os.ReadFile(h.manifestPath(room)); err == nil {
		if err := json.Unmarshal(data, &old); err != nil {
			return syncfs.Plan{}, fmt.Errorf("read manifest.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return syncfs.Plan{}, err
	}
	plan, err := syncfs.MakePlan(h.workDir(room), old, next)
	if err != nil {
		return plan, err
	}
	data, err := json.Marshal(storedPlan{Plan: plan, Manifest: next})
	if err != nil {
		return plan, err
	}
	return plan, writeFileAtomic(filepath.Join(h.roomDir(room), "plans", plan.PlanID+".json"), data, 0o600)
}

func (h *helper) syncApply(args []string, stdin io.Reader) (map[string]int, error) {
	room, pos, err := roomFlag("sync-apply", args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 || strings.ContainsAny(pos[0], "/\\.") {
		return nil, fmt.Errorf("%w: sync-apply --room <room> <plan-id>", cli.ErrUsage)
	}
	planFile := filepath.Join(h.roomDir(room), "plans", pos[0]+".json")
	data, err := os.ReadFile(planFile)
	if err != nil {
		return nil, fmt.Errorf("no sync plan %s; run the sync again", pos[0])
	}
	var sp storedPlan
	if err := json.Unmarshal(data, &sp); err != nil {
		return nil, err
	}
	written, deleted, err := syncfs.Apply(h.workDir(room), sp.Plan, sp.Manifest, stdin)
	if err != nil {
		return nil, err
	}
	manifest, err := json.Marshal(sp.Manifest)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(h.manifestPath(room), manifest, 0o600); err != nil {
		return nil, err
	}
	os.Remove(planFile)
	paths := make([]string, 0, len(sp.Manifest.Entries))
	for _, e := range sp.Manifest.Entries {
		paths = append(paths, e.Path)
	}
	if err := h.baselineCommit(room, paths, sp.Plan.Delete); err != nil {
		return nil, fmt.Errorf("baseline commit: %w", err)
	}
	return map[string]int{"written": written, "deleted": deleted}, nil
}

// git runs git on the baseline: a separate git directory, so work/ has no .git.
// The user's git configuration is not used.
func (h *helper) git(room string, stdin io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_DIR="+filepath.Join(h.roomDir(room), "baseline.git"),
		"GIT_WORK_TREE="+h.workDir(room),
		"GIT_LITERAL_PATHSPECS=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=hostel", "GIT_AUTHOR_EMAIL=hostel@localhost",
		"GIT_COMMITTER_NAME=hostel", "GIT_COMMITTER_EMAIL=hostel@localhost",
	)
	cmd.Dir = h.workDir(room)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

func nulList(paths []string) io.Reader {
	var b bytes.Buffer
	for _, p := range paths {
		b.WriteString(p)
		b.WriteByte(0)
	}
	return &b
}

// baselineCommit stages exactly the manifest paths and the deleted paths, and
// commits. Without git on the machine, there is no baseline.
func (h *helper) baselineCommit(room string, paths, deleted []string) error {
	if !gitAvailable() {
		return nil
	}
	if _, err := os.Stat(filepath.Join(h.roomDir(room), "baseline.git")); errors.Is(err, os.ErrNotExist) {
		if _, err := h.git(room, nil, "init", "-q"); err != nil {
			return err
		}
	}
	if len(deleted) > 0 {
		if _, err := h.git(room, nulList(deleted), "rm", "-q", "--cached", "--ignore-unmatch", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return err
		}
	}
	if len(paths) > 0 {
		// -f: include files such as .env are ignored files.
		if _, err := h.git(room, nulList(paths), "add", "-f", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return err
		}
	}
	_, err := h.git(room, nil, "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "--no-verify", "-m", "hostel sync")
	return err
}

// diff prints the changes against the last baseline commit: changed manifest
// files and new files that the repository's .gitignore does not ignore.
func (h *helper) diff(args []string, stdout io.Writer) error {
	room, _, err := roomFlag("diff", args)
	if err != nil {
		return err
	}
	if !gitAvailable() {
		return errors.New("diff is not available: git is not installed on the machine")
	}
	if _, err := os.Stat(filepath.Join(h.roomDir(room), "baseline.git")); err != nil {
		return errors.New("no baseline yet: run \"hostel sync\" first")
	}
	out, err := h.git(room, nil, "diff", "--no-color", "HEAD")
	if err != nil {
		return err
	}
	stdout.Write(out)
	others, err := h.git(room, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	for _, p := range strings.Split(strings.TrimRight(string(others), "\x00"), "\x00") {
		if p == "" {
			continue
		}
		// git diff --no-index exits with 1 when the files differ.
		out, _ := h.git(room, nil, "diff", "--no-color", "--no-index", "--", "/dev/null", p)
		stdout.Write(out)
	}
	return nil
}
