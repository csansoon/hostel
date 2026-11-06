// Package guest is the guest side of Hostel: the commands that agents use.
package guest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
)

// Repo is the git worktree that the guest runs in.
type Repo struct {
	Root   string // top-level directory
	GitDir string // per-worktree git directory
}

func gitOutput(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// FindRepo finds the git worktree of the current directory.
func FindRepo() (*Repo, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	root, err := gitOutput(wd, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("hostel needs a git repository: %w", err)
	}
	gitDir, err := gitOutput(wd, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	return &Repo{Root: root, GitDir: gitDir}, nil
}

// RepoConfig reads hostel.toml in the repository root; without it, the
// repository has no tunnels, includes, or setup commands.
func (r *Repo) RepoConfig() (*config.Repo, error) {
	file := filepath.Join(r.Root, "hostel.toml")
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return &config.Repo{}, nil
	}
	return config.LoadRepo(file)
}

func (r *Repo) stateRoot() string { return filepath.Join(r.GitDir, "hostel") }

// State is the local state of the worktree's lease (one lease for each
// worktree): $(git rev-parse --git-dir)/hostel/lease/.
type State struct {
	Dir string `json:"-"`

	LeaseID string `json:"lease_id"`
	// Tools is the request.
	Tools []string `json:"tools"`
	// Root is the repository root; the port process reads .env there.
	Root     string `json:"root"`
	Task     string `json:"task"`
	Priority string `json:"priority"`
	Fresh    bool   `json:"fresh"`
	Final    bool   `json:"final"`
	// Machine ("<provider>/<machine>") and Started, the screen tools that
	// started for the lease, come from the server when the lease is ready.
	Machine string                 `json:"machine,omitempty"`
	Started []protocol.StartedTool `json:"started,omitempty"`
	// Step is the last acquire step that finished.
	Step   string           `json:"step"`
	Access *protocol.Access `json:"access,omitempty"`
	// SetupJob and SetupOffset let a repeated acquire continue the setup job.
	SetupJob    string `json:"setup_job,omitempty"`
	SetupOffset int64  `json:"setup_offset,omitempty"`
	// JobOffsets has the output bytes that run and attach already printed for
	// each job, so a later attach continues there.
	JobOffsets map[string]int64 `json:"job_offsets,omitempty"`
}

// Acquire steps, in order.
const (
	StepCreated = "created"
	StepEnv     = "env"
	StepPorts   = "ports"
	StepSynced  = "synced"
	StepSetup   = "setup"
	StepDone    = "done"
)

func (s *State) path() string    { return filepath.Join(s.Dir, "lease.json") }
func (s *State) KeyPath() string { return filepath.Join(s.Dir, "id_ed25519") }

// LoadState reads the state of the worktree's lease; ok is false if there is
// none.
func (r *Repo) LoadState() (*State, bool, error) {
	return loadStateDir(filepath.Join(r.stateRoot(), "lease"))
}

// leaseTools are the tools of the lease for hostel.toml: the requested ones
// and their needs, each once (from the server).
func (s *State) leaseTools() []string {
	if s.Access == nil {
		return nil
	}
	return s.Access.LeaseTools
}

// Save writes the state with a temporary file and a rename.
func (s *State) Save() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

// NewKey creates the lease key pair and returns the public key line.
func (s *State) NewKey() (string, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "hostel-lease")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(s.KeyPath(), pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))), nil
}

func (s *State) signer() (ssh.Signer, error) {
	data, err := os.ReadFile(s.KeyPath())
	if err != nil {
		return nil, fmt.Errorf("read the lease key: %w", err)
	}
	return ssh.ParsePrivateKey(data)
}

// ActiveState returns the state of the worktree's lease, if it has not
// ended.
func (r *Repo) ActiveState() (*State, error) {
	s, ok, err := r.LoadState()
	if err != nil {
		return nil, err
	}
	if !ok || s.Final {
		return nil, errors.New("no active lease in this worktree; run \"hostel acquire [<tool>...] --task ...\"")
	}
	return s, nil
}

// currentBranch is the git branch of the worktree for the status (empty when
// it is not known, for example a detached HEAD).
func currentBranch(dir string) string {
	// 5 s: the branch is only for the status; a slow git must not hold up
	// the lease.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "branch", "--show-current")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
