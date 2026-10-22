package room

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

func (h *helper) manifestPath(slot string) string {
	return filepath.Join(h.slotDir(slot), "manifest.json")
}

func (h *helper) syncPlan(args []string, stdin io.Reader) (syncfs.Plan, error) {
	slot, _, err := slotFlag("sync-plan", args)
	if err != nil {
		return syncfs.Plan{}, err
	}
	if _, err := h.readLease(slot); err != nil {
		return syncfs.Plan{}, err
	}
	var next syncfs.Manifest
	if err := json.NewDecoder(stdin).Decode(&next); err != nil {
		return syncfs.Plan{}, fmt.Errorf("read the manifest: %w", err)
	}
	var old syncfs.Manifest
	if data, err := os.ReadFile(h.manifestPath(slot)); err == nil {
		if err := json.Unmarshal(data, &old); err != nil {
			return syncfs.Plan{}, fmt.Errorf("read manifest.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return syncfs.Plan{}, err
	}
	plan, err := syncfs.MakePlan(h.workDir(slot), old, next)
	if err != nil {
		return plan, err
	}
	data, err := json.Marshal(storedPlan{Plan: plan, Manifest: next})
	if err != nil {
		return plan, err
	}
	return plan, writeFileAtomic(filepath.Join(h.slotDir(slot), "plans", plan.PlanID+".json"), data, 0o600)
}

func (h *helper) syncApply(args []string, stdin io.Reader) (map[string]int, error) {
	slot, pos, err := slotFlag("sync-apply", args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 1 || strings.ContainsAny(pos[0], "/\\.") {
		return nil, fmt.Errorf("%w: sync-apply --slot <slot> <plan-id>", cli.ErrUsage)
	}
	planFile := filepath.Join(h.slotDir(slot), "plans", pos[0]+".json")
	data, err := os.ReadFile(planFile)
	if err != nil {
		return nil, fmt.Errorf("no sync plan %s; run the sync again", pos[0])
	}
	var sp storedPlan
	if err := json.Unmarshal(data, &sp); err != nil {
		return nil, err
	}
	written, deleted, err := syncfs.Apply(h.workDir(slot), sp.Plan, sp.Manifest, stdin)
	if err != nil {
		return nil, err
	}
	manifest, err := json.Marshal(sp.Manifest)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(h.manifestPath(slot), manifest, 0o600); err != nil {
		return nil, err
	}
	os.Remove(planFile)
	paths := make([]string, 0, len(sp.Manifest.Entries))
	for _, e := range sp.Manifest.Entries {
		paths = append(paths, e.Path)
	}
	if err := h.baselineCommit(slot, paths, sp.Plan.Delete); err != nil {
		return nil, fmt.Errorf("baseline commit: %w", err)
	}
	return map[string]int{"written": written, "deleted": deleted}, nil
}

// git runs git on the baseline: a separate git directory, so work/ has no .git.
// The user's git configuration is not used.
func (h *helper) git(slot string, stdin io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(),
		"GIT_DIR="+filepath.Join(h.slotDir(slot), "baseline.git"),
		"GIT_WORK_TREE="+h.workDir(slot),
		"GIT_LITERAL_PATHSPECS=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=hostel", "GIT_AUTHOR_EMAIL=hostel@localhost",
		"GIT_COMMITTER_NAME=hostel", "GIT_COMMITTER_EMAIL=hostel@localhost",
	)
	cmd.Dir = h.workDir(slot)
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
// commits. Without git on the room, there is no baseline.
func (h *helper) baselineCommit(slot string, paths, deleted []string) error {
	if !gitAvailable() {
		return nil
	}
	if _, err := os.Stat(filepath.Join(h.slotDir(slot), "baseline.git")); errors.Is(err, os.ErrNotExist) {
		if _, err := h.git(slot, nil, "init", "-q"); err != nil {
			return err
		}
	}
	if len(deleted) > 0 {
		if _, err := h.git(slot, nulList(deleted), "rm", "-q", "--cached", "--ignore-unmatch", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return err
		}
	}
	if len(paths) > 0 {
		// -f: include files such as .env are ignored files.
		if _, err := h.git(slot, nulList(paths), "add", "-f", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return err
		}
	}
	_, err := h.git(slot, nil, "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "--no-verify", "-m", "hostel sync")
	return err
}

// diff prints the changes against the last baseline commit: changed manifest
// files and new files that the repository's .gitignore does not ignore.
func (h *helper) diff(args []string, stdout io.Writer) error {
	slot, _, err := slotFlag("diff", args)
	if err != nil {
		return err
	}
	if !gitAvailable() {
		return errors.New("diff is not available: git is not installed on the room")
	}
	if _, err := os.Stat(filepath.Join(h.slotDir(slot), "baseline.git")); err != nil {
		return errors.New("no baseline yet: run \"hostel sync\" first")
	}
	out, err := h.git(slot, nil, "diff", "--no-color", "HEAD")
	if err != nil {
		return err
	}
	stdout.Write(out)
	others, err := h.git(slot, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	for _, p := range strings.Split(strings.TrimRight(string(others), "\x00"), "\x00") {
		if p == "" {
			continue
		}
		// git diff --no-index exits with 1 when the files differ.
		out, _ := h.git(slot, nil, "diff", "--no-color", "--no-index", "--", "/dev/null", p)
		stdout.Write(out)
	}
	return nil
}
