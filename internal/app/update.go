package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/update"
)

const (
	// latestEvery is the time between two reads of the latest release;
	// latestTimeout bounds one read.
	latestEvery   = 6 * time.Hour
	latestTimeout = 15 * time.Second
)

type (
	// latestMsg is the tag of the latest release ("" after an error: the
	// check is silent).
	latestMsg   struct{ tag string }
	latestTick  struct{}
	selfUpdated struct {
		text string
		err  error
	}
)

// checkLatest reads the latest release tag in the background (owner
// decision, 2026-11-04). HOSTEL_NO_UPDATE_CHECK=1 turns it off.
func (m *model) checkLatest() tea.Cmd {
	if os.Getenv("HOSTEL_NO_UPDATE_CHECK") == "1" {
		return nil
	}
	return func() tea.Msg {
		src, err := update.Default()
		if err != nil {
			return latestMsg{}
		}
		ctx, cancel := context.WithTimeout(context.Background(), latestTimeout)
		defer cancel()
		tag, err := src.Latest(ctx)
		if err != nil {
			return latestMsg{}
		}
		return latestMsg{tag: tag}
	}
}

// updateAvailable is the newer release for this computer ("" for none).
func (m *model) updateAvailable() string {
	if m.updated || !update.Newer(m.latest, m.version) {
		return ""
	}
	return m.latest
}

// serverUpdateAvailable is the newer release for the server, for admins (""
// for none).
func (m *model) serverUpdateAvailable() string {
	if !m.admin() || !update.Newer(m.latest, m.info.Version) {
		return ""
	}
	return m.latest
}

// runSelfUpdate installs the latest release in place of this binary.
func (m *model) runSelfUpdate(tag string) tea.Cmd {
	version := m.version
	return func() tea.Msg {
		_, err := update.Self(context.Background(), version, tag, false)
		if err != nil {
			return selfUpdated{err: err}
		}
		return selfUpdated{text: fmt.Sprintf("Hostel %s is installed. Quit (q) and run hostel again.", tag)}
	}
}

// startServerUpdate asks the server to update itself to tag.
func (m *model) startServerUpdate(tag string) tea.Cmd {
	c, gen := m.c, m.gen
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		var st protocol.ServerUpdate
		if err := c.Do(ctx, http.MethodPost, "/v1/server/update", protocol.ServerUpdateRequest{Version: tag}, &st); err != nil {
			return actionMsg{gen: gen, err: err}
		}
		return actionMsg{gen: gen, text: fmt.Sprintf("The server updates from %s to %s. It starts again by itself; then Reception shows the new version.", st.From, st.To)}
	}
}

// outOfDate reports a machine whose connect service is older than the
// server.
func (m *model) outOfDate(ms protocol.MachineStatus) bool {
	return ms.Connect && update.Older(ms.ConnectVersion, m.info.Version)
}

// machineUpdateCommand is the command that updates the connect service of a
// machine, as an administrator on it.
func machineUpdateCommand(osName string) string {
	if osName == "windows" {
		return `C:\ProgramData\hostel\hostel.exe machine update`
	}
	return "sudo /etc/hostel/hostel machine update"
}
