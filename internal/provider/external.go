package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/csansoon/hostel/internal/config"
)

// External runs a command that implements the JSON contract of design
// section 7 ("External provider contract").
type External struct {
	cfg       *config.Provider
	serverKey ssh.Signer
	log       *slog.Logger

	mu   sync.Mutex
	caps map[string]Capabilities
}

func NewExternal(cfg *config.Provider, serverKey ssh.Signer, log *slog.Logger) *External {
	return &External{cfg: cfg, serverKey: serverKey, log: log, caps: map[string]Capabilities{}}
}

func (e *External) Kind() string { return config.ProviderExternal }

type externalError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// call runs "command... <op>" with one JSON object on stdin and decodes one
// JSON object from stdout. stderr goes to the debug log; provider commands
// must not print secrets.
func (e *External) call(ctx context.Context, op string, in, out any) error {
	timeout := e.cfg.Timeouts.Default.D()
	if op == "provision" {
		timeout = e.cfg.Timeouts.Provision.D()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, e.cfg.Command[0], append(e.cfg.Command[1:], op)...)
	cmd.Env = os.Environ()
	for k, v := range e.cfg.CommandEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if stderr.Len() > 0 {
		e.log.Debug("provider stderr", "provider", e.cfg.Name, "op", op, "stderr", stderr.String())
	}
	if ctx.Err() != nil {
		return fmt.Errorf("provider %s %s: timed out after %s", e.cfg.Name, op, timeout)
	}
	if runErr != nil {
		var pe externalError
		if json.Unmarshal(stdout.Bytes(), &pe) == nil && pe.Error != "" {
			switch pe.Error {
			case "limit":
				return fmt.Errorf("provider %s %s: %w: %s", e.cfg.Name, op, ErrLimit, pe.Message)
			case "not_found":
				return fmt.Errorf("provider %s %s: %w: %s", e.cfg.Name, op, ErrNotFound, pe.Message)
			}
			return fmt.Errorf("provider %s %s: %s", e.cfg.Name, op, pe.Message)
		}
		return fmt.Errorf("provider %s %s: %v: %s", e.cfg.Name, op, runErr, strings.TrimSpace(stdout.String()))
	}
	if out != nil {
		if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
			return fmt.Errorf("provider %s %s: decode the response: %w", e.cfg.Name, op, err)
		}
	}
	return nil
}

// LoadCapabilities asks the command for the capabilities of each machine. The
// server calls it at start.
func (e *External) LoadCapabilities(ctx context.Context) error {
	for _, m := range e.cfg.TemplateOrder {
		var resp struct {
			CanStop   bool `json:"can_stop"`
			CanExtend bool `json:"can_extend"`
		}
		if err := e.call(ctx, "capabilities", map[string]string{"template": m}, &resp); err != nil {
			return err
		}
		e.mu.Lock()
		e.caps[m] = Capabilities{OnDemand: true, CanStop: resp.CanStop, CanExtend: resp.CanExtend}
		e.mu.Unlock()
	}
	return nil
}

func (e *External) Capabilities(template string) Capabilities {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.caps[template]
	c.OnDemand = true
	return c
}

type externalAccess struct {
	ID           string   `json:"id"`
	User         string   `json:"user"`
	SSH          *sshAddr `json:"ssh"`
	ProxyCommand []string `json:"proxy_command"`
}

type sshAddr struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (e *External) Provision(ctx context.Context, req ProvisionRequest) (MachineRef, error) {
	var resp externalAccess
	err := e.call(ctx, "provision", map[string]any{"name": req.Name, "template": req.Template, "spec": req.Spec,
		"deadline": req.Deadline.UTC().Format(time.RFC3339), "server_key": req.ServerKey}, &resp)
	if err != nil {
		return MachineRef{}, err
	}
	if resp.ID == "" {
		return MachineRef{}, fmt.Errorf("provider %s provision: the response has no id", e.cfg.Name)
	}
	return MachineRef{Name: req.Name, Template: req.Template, ID: resp.ID}, nil
}

func (e *External) Resume(ctx context.Context, machine MachineRef) error {
	return e.call(ctx, "resume", map[string]string{"id": machine.ID}, nil)
}

func (e *External) Stop(ctx context.Context, machine MachineRef) error {
	return e.call(ctx, "stop", map[string]string{"id": machine.ID}, nil)
}

func (e *External) Extend(ctx context.Context, machine MachineRef, deadline time.Time) error {
	return e.call(ctx, "extend", map[string]string{"id": machine.ID, "deadline": deadline.UTC().Format(time.RFC3339)}, nil)
}

func (e *External) Destroy(ctx context.Context, machine MachineRef) error {
	return e.call(ctx, "destroy", map[string]string{"id": machine.ID, "name": machine.Name}, nil)
}

func (e *External) List(ctx context.Context, prefix string) ([]MachineRef, error) {
	var resp struct {
		Machines []struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"machines"`
	}
	if err := e.call(ctx, "list", map[string]string{"prefix": prefix}, &resp); err != nil {
		return nil, err
	}
	var out []MachineRef
	for _, r := range resp.Machines {
		if strings.HasPrefix(r.Name, prefix) {
			out = append(out, MachineRef{Name: r.Name, ID: r.ID, State: r.State})
		}
	}
	return out, nil
}

func (e *External) Access(ctx context.Context, machine MachineRef) (MachineAccess, error) {
	var resp externalAccess
	if err := e.call(ctx, "access", map[string]string{"id": machine.ID}, &resp); err != nil {
		return MachineAccess{}, err
	}
	a := MachineAccess{User: resp.User, Signers: []ssh.Signer{e.serverKey}}
	switch {
	case resp.SSH != nil:
		a.Dial = TCPDial(net.JoinHostPort(resp.SSH.Host, strconv.Itoa(resp.SSH.Port)))
	case len(resp.ProxyCommand) > 0:
		a.Dial = proxyDial(resp.ProxyCommand)
	default:
		return MachineAccess{}, errors.New("provider access: the response has neither ssh nor proxy_command")
	}
	if a.User == "" {
		return MachineAccess{}, errors.New("provider access: the response has no user")
	}
	return a, nil
}
