package helper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// shareCache on Windows: the group hostel-leases gets read access to
// hostel\bin, tools, mise, and cache (inherited by their content), so that a
// lease user copies them and reads the cache; it gets nothing else. Windows
// lets every user pass through the directories above them.
func (h *helper) shareCache() (struct{}, error) {
	icacls := filepath.Join(os.Getenv("SystemRoot"), "System32", "icacls.exe")
	for _, d := range []string{"bin", "tools", "mise", "cache"} {
		p := filepath.Join(h.root, d)
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		out, err := exec.CommandContext(ctx, icacls, p, "/grant", "hostel-leases:(OI)(CI)RX").CombinedOutput()
		cancel()
		if err != nil {
			return struct{}{}, fmt.Errorf("icacls %s: %v: %s", p, err, strings.TrimSpace(string(out)))
		}
	}
	return struct{}{}, nil
}
