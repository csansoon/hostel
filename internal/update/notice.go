package update

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// noticeEvery is the shortest time between two notices.
const noticeEvery = 24 * time.Hour

// Notice prints one line when the server runs a newer version than this
// computer, at most once in noticeEvery; stateFile keeps the time of the last
// notice.
func Notice(w io.Writer, server, have, stateFile string) {
	if !Older(have, server) {
		return
	}
	if data, err := os.ReadFile(stateFile); err == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data))); err == nil && time.Since(t) < noticeEvery {
			return
		}
	}
	os.MkdirAll(filepath.Dir(stateFile), 0o700)
	if err := os.WriteFile(stateFile, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return // without the state, the notice would show on each command
	}
	fmt.Fprintf(w, "hostel: the server runs Hostel %s and this computer has %s; run \"hostel update\"\n", server, have)
}
