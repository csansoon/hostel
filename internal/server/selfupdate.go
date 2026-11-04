package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/update"
	"github.com/csansoon/hostel/internal/version"
)

// ErrRestart: Run stopped for a restart with the new binary (after a remote
// update); the caller starts it in the same process.
var ErrRestart = errors.New("restart with the new binary")

// updateTimeout bounds a remote update (the downloads of all helper files).
const updateTimeout = 20 * time.Minute

// selfUpdate is the state of the remote update (one at a time).
type selfUpdate struct {
	mu    sync.Mutex
	state protocol.ServerUpdate
}

// handleServerUpdate starts a remote update (POST /v1/server/update, admin):
// it reads the release, checks that it is newer, and returns 202; the
// download and the restart run in the background.
func (s *Server) handleServerUpdate(w http.ResponseWriter, r *http.Request) {
	var req protocol.ServerUpdateRequest
	if r.ContentLength != 0 && !decodeBody(w, r, &req) {
		return
	}
	if runtime.GOOS == "windows" {
		writeError(w, http.StatusNotImplemented, "", "a server on Windows cannot restart itself; update it on the server machine")
		return
	}
	s.update.mu.Lock()
	running := s.update.state.State == protocol.UpdateRunning
	if !running {
		s.update.state = protocol.ServerUpdate{State: protocol.UpdateRunning, From: version.Version}
	}
	s.update.mu.Unlock()
	if running {
		writeError(w, http.StatusConflict, "", "an update of the server runs already")
		return
	}
	fail := func(status int, err error) {
		s.update.mu.Lock()
		s.update.state = protocol.ServerUpdate{State: protocol.UpdateIdle}
		s.update.mu.Unlock()
		writeError(w, status, "", err.Error())
	}
	src, err := update.Default()
	if err != nil {
		fail(http.StatusInternalServerError, err)
		return
	}
	tag := req.Version
	if tag == "" {
		if tag, err = src.Latest(r.Context()); err != nil {
			fail(http.StatusBadGateway, err)
			return
		}
	}
	if _, known := update.Parse(version.Version); known && !update.Newer(tag, version.Version) {
		fail(http.StatusConflict, fmt.Errorf("the server has %s; %s is not newer", version.Version, tag))
		return
	}
	s.update.mu.Lock()
	s.update.state.To = tag
	st := s.update.state
	s.update.mu.Unlock()
	s.log.Info("server update started", "from", version.Version, "to", tag, "by", requestToken(r).Name)
	go s.runUpdate(src, tag)
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) runUpdate(src *update.Source, tag string) {
	ctx, cancel := context.WithTimeout(s.runCtx, updateTimeout)
	defer cancel()
	err := func() error {
		rel, err := src.Release(ctx, tag)
		if err != nil {
			return err
		}
		return update.Server(ctx, src, rel, s.exe, s.config().HelperDir)
	}()
	s.update.mu.Lock()
	defer s.update.mu.Unlock()
	if err != nil {
		s.log.Error("server update failed", "to", tag, "err", err)
		s.update.state.State, s.update.state.Error = protocol.UpdateFailed, err.Error()
		return
	}
	if s.runCtx.Err() != nil {
		return // the server stops; the new binary starts at its next start
	}
	s.log.Info("server updated; restarting", "from", version.Version, "to", tag)
	s.restart()
}

// handleServerUpdateState is GET /v1/server/update (admin).
func (s *Server) handleServerUpdateState(w http.ResponseWriter, r *http.Request) {
	s.update.mu.Lock()
	st := s.update.state
	s.update.mu.Unlock()
	if st.State == "" {
		st.State = protocol.UpdateIdle
	}
	writeJSON(w, http.StatusOK, st)
}
