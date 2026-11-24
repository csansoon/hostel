//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/shell"
	"github.com/csansoon/hostel/internal/update"
)

// TestSmokeWindowsRemoteUpdate runs against a real Windows machine that joined
// a server with v0.5.0 or later (the SYSTEM service of the root step), as
// TestSmokeWindows (HOSTEL_SMOKE_WINDOWS, HOSTEL_SMOKE_WINDOWS_KEY: the machine
// user, PowerShell default shell). The Docker machines are Linux, so only this
// test checks that the machine user can start the server's command of the root
// step (M13). The step asks the server that the machine joined: with the same
// release it answers "up to date" and restarts the connection; with a newer
// one it updates the machine.
func TestSmokeWindowsRemoteUpdate(t *testing.T) {
	addr, keyFile := os.Getenv("HOSTEL_SMOKE_WINDOWS"), os.Getenv("HOSTEL_SMOKE_WINDOWS_KEY")
	if addr == "" || keyFile == "" {
		t.Skip("set HOSTEL_SMOKE_WINDOWS=<user@host> and HOSTEL_SMOKE_WINDOWS_KEY to run it against a Windows machine")
	}
	cmd, err := shell.Command(shell.PowerShell, update.WindowsRootStep, "machine", "update", "--signed")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-T", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-i", keyFile, addr, cmd).CombinedOutput()
	if err != nil {
		t.Fatalf("the root step as the machine user: %v\n%s", err, out)
	}
	if got := string(out); !strings.Contains(got, "up to date") && !strings.Contains(got, "updated") {
		t.Fatalf("the root step's result: %s", got)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}
