package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// loginTimeout ends a login that the admin does not complete.
const loginTimeout = 10 * time.Minute

// loginCLIs are the provider CLIs that log in with "login --browser=false":
// it prints an address to open on any machine and ends when the login is
// complete (docs/namespace.md). The credentials stay on the server.
var loginCLIs = map[string]bool{"nsc": true, "devbox": true}

var urlRE = regexp.MustCompile(`https://\S+`)

// logins are the logins of the provider CLIs, one at a time for each CLI.
type logins struct {
	mu  sync.Mutex
	cur map[string]*protocol.Login
	// changed is closed and replaced when a login changes.
	changed chan struct{}
}

func (l *logins) get(cli string) (protocol.Login, chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.changed == nil {
		l.changed = make(chan struct{})
	}
	if c := l.cur[cli]; c != nil {
		return *c, l.changed
	}
	return protocol.Login{CLI: cli}, l.changed
}

// begin marks a login of cli as running; false if one runs already.
func (l *logins) begin(cli string) bool {
	started := false
	l.set(cli, func(c *protocol.Login) {
		if c.State != protocol.LoginWaiting {
			*c = protocol.Login{CLI: cli, State: protocol.LoginWaiting}
			started = true
		}
	})
	return started
}

func (l *logins) set(cli string, f func(*protocol.Login)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cur == nil {
		l.cur = map[string]*protocol.Login{}
	}
	if l.cur[cli] == nil {
		l.cur[cli] = &protocol.Login{CLI: cli}
	}
	f(l.cur[cli])
	if l.changed != nil {
		close(l.changed)
	}
	l.changed = make(chan struct{})
}

// handleLogin starts a login of a provider CLI (or reports that it is logged
// in) and answers when the address is known.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	cli := r.PathValue("cli")
	if !loginCLIs[cli] {
		writeError(w, http.StatusNotFound, "", fmt.Sprintf("no login for %q; the logins are devbox and nsc (Namespace)", cli))
		return
	}
	if _, err := exec.LookPath(cli); err != nil {
		writeError(w, http.StatusNotFound, "not_installed", fmt.Sprintf("%s is not installed on the server (docs/deploy-vps.md, section 5)", cli))
		return
	}
	cur, _ := s.logins.get(cli)
	if cur.State != protocol.LoginWaiting {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		err := exec.CommandContext(ctx, cli, "auth", "check-login").Run()
		cancel()
		if err == nil {
			writeJSON(w, http.StatusOK, protocol.Login{CLI: cli, State: protocol.LoginDone, Already: true})
			return
		}
		if s.logins.begin(cli) {
			go s.runLogin(cli, requestToken(r).Name)
		}
	}
	// Wait for the address (or the end of the login).
	deadline := time.After(30 * time.Second)
	for {
		l, changed := s.logins.get(cli)
		if l.URL != "" || l.State != protocol.LoginWaiting {
			writeJSON(w, http.StatusOK, l)
			return
		}
		select {
		case <-changed:
		case <-deadline:
			writeError(w, http.StatusGatewayTimeout, "", fmt.Sprintf("%s login printed no address in 30 s", cli))
			return
		case <-r.Context().Done():
			return
		}
	}
}

// runLogin runs "<cli> login --browser=false" (its own deadline: the request
// that started it ends sooner).
func (s *Server) runLogin(cli, admin string) {
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, "login", "--browser=false")
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	cmd.WaitDelay = 5 * time.Second
	var tail []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line := sc.Text()
			if u := urlRE.FindString(line); u != "" {
				s.logins.set(cli, func(l *protocol.Login) {
					if l.URL == "" {
						l.URL = u
					}
				})
				continue
			}
			// The output without addresses, for the error (an address can be
			// a one-time login link).
			tail = append(tail, line)
			if len(tail) > 5 {
				tail = tail[1:]
			}
		}
	}()
	err := cmd.Start()
	if err == nil {
		err = cmd.Wait()
	}
	pw.Close()
	<-done
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("not completed in %s", loginTimeout)
	}
	s.logins.set(cli, func(l *protocol.Login) {
		if err != nil {
			l.State, l.Error = protocol.LoginFailed, fmt.Sprintf("%s login: %v: %s", cli, err, strings.Join(tail, " / "))
		} else {
			l.State = protocol.LoginDone
		}
	})
	s.log.Info("provider login", "cli", cli, "admin", admin, "ok", err == nil)
}

// handleLoginState answers the state of a login; with ?wait=1 it waits up to
// 30 s for a change.
func (s *Server) handleLoginState(w http.ResponseWriter, r *http.Request) {
	cli := r.PathValue("cli")
	l, changed := s.logins.get(cli)
	if r.URL.Query().Get("wait") != "" && l.State == protocol.LoginWaiting {
		select {
		case <-changed:
		case <-time.After(30 * time.Second):
		case <-r.Context().Done():
			return
		}
		l, _ = s.logins.get(cli)
	}
	writeJSON(w, http.StatusOK, l)
}
