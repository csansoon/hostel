//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// smokeMacApp is a small AppKit app: a button that counts and a text field.
const smokeMacApp = `import AppKit

final class Delegate: NSObject, NSApplicationDelegate {
    var window: NSWindow!
    let label = NSTextField(labelWithString: "Count: 0")
    let field = NSTextField(string: "")
    var count = 0
    func applicationDidFinishLaunching(_ n: Notification) {
        window = NSWindow(contentRect: NSRect(x: 200, y: 300, width: 320, height: 160),
                          styleMask: [.titled, .closable], backing: .buffered, defer: false)
        window.title = "Hostel Smoke"
        let button = NSButton(title: "Add one", target: self, action: #selector(add))
        button.setAccessibilityIdentifier("add")
        label.setAccessibilityIdentifier("count")
        field.setAccessibilityIdentifier("name")
        let stack = NSStackView(views: [field, label, button])
        stack.orientation = .vertical
        stack.frame = window.contentView!.bounds
        window.contentView!.addSubview(stack)
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }
    @objc func add() { count += 1; label.stringValue = "Count: \(count)" }
}

let app = NSApplication.shared
let delegate = Delegate()
app.delegate = delegate
app.setActivationPolicy(.regular)
app.run()
`

// smokeMacBuild builds smokeMacApp into HostelSmoke.app (an ad hoc signed bundle).
const smokeMacBuild = `set -e
mkdir -p HostelSmoke.app/Contents/MacOS
swiftc -O main.swift -o HostelSmoke.app/Contents/MacOS/HostelSmoke
/usr/libexec/PlistBuddy -c "Add :CFBundleIdentifier string so.hostel.smoke" -c "Add :CFBundleExecutable string HostelSmoke" -c "Add :CFBundlePackageType string APPL" HostelSmoke.app/Contents/Info.plist
codesign -s - --force HostelSmoke.app`

// TestSmokeNamespaceDesktop (M7) runs against the real Namespace account and
// costs money (a macOS devbox for about 15 minutes). It runs only with
// HOSTEL_SMOKE_NAMESPACE=1 ("make test-smoke-namespace").
//
// A desktop lease builds a small app, starts it, reads it, clicks and types in
// it with agent-device, and takes a screenshot. It also leaves a copy in
// ~/Applications and TextEdit with unsaved text. A second lease on the same
// warm devbox finds none of them. HOSTEL_SMOKE_OUT (optional) is a directory
// for the screenshot.
func TestSmokeNamespaceDesktop(t *testing.T) {
	if os.Getenv("HOSTEL_SMOKE_NAMESPACE") != "1" {
		t.Skip("set HOSTEL_SMOKE_NAMESPACE=1 to run it against the real Namespace account (it costs money)")
	}
	e, s, prefix := namespaceDevboxSmoke(t, `warm_running = "10m"`, `
[providers.ns.machines.mac]
spec = { product = "devbox", platform = "macos", size = "m", image = "tahoe-slim" }
labels = ["desktop"]
max_lifetime = "3h"
`, true)
	e.newRepo(map[string]string{"main.swift": smokeMacApp})

	acquire := func() string {
		start := time.Now()
		for {
			r := e.hostel("acquire", "macos", "--require", "desktop", "--task", "desktop smoke test", "--wait", "1m")
			if r.code == 0 {
				t.Logf("lease ready after %s", time.Since(start).Round(time.Second))
				for _, room := range s.status().Rooms {
					if strings.HasPrefix(room.Name, prefix) {
						return room.Name
					}
				}
				t.Fatal("no room of the test in the status")
			}
			if r.code != 75 || time.Since(start) > 20*time.Minute {
				t.Fatalf("acquire: %s", r)
			}
		}
	}
	// agent runs agent-device; the runner can still drain an earlier command
	// (m7-implementation.md, section 1), so a failed command is repeated. Only
	// for commands that are safe to repeat.
	agent := func(args ...string) result {
		t.Helper()
		var r result
		for range 4 {
			r = e.hostel("run", "--", "agent-device "+strings.Join(args, " ")+" --platform macos")
			if r.code == 0 {
				return r
			}
			time.Sleep(5 * time.Second)
		}
		t.Fatalf("agent-device %s: %s", args[0], r)
		return r
	}
	room := acquire()

	e.mustHostel("run", "--", smokeMacBuild)
	e.mustHostel("run", "--", "open ./HostelSmoke.app")
	agent("open", "so.hostel.smoke")
	if r := agent("snapshot", "-i"); !strings.Contains(r.stdout, "Count: 0") {
		t.Fatalf("snapshot of the app: %s", r.stdout)
	}
	agent("click", `"id=add"`)
	agent("click", `"id=add"`)
	if r := agent("get", "text", `"id=count"`); !strings.Contains(r.stdout, "Count: 2") {
		t.Fatalf("label after two clicks: %s", r.stdout)
	}
	agent("fill", `"id=name"`, `"typed by hostel"`)
	if r := agent("get", "text", `"id=name"`); !strings.Contains(r.stdout, "typed by hostel") {
		t.Fatalf("text field after fill: %s", r.stdout)
	}
	shot := filepath.Join(t.TempDir(), "desktop.png")
	if dir := os.Getenv("HOSTEL_SMOKE_OUT"); dir != "" {
		shot = filepath.Join(dir, "smoke-macos-desktop.png")
	}
	e.mustHostel("screenshot", "-o", shot)
	if data, err := os.ReadFile(shot); err != nil || !bytes.HasPrefix(data, []byte("\x89PNG")) || len(data) < 10000 {
		t.Fatalf("desktop screenshot: %d bytes, %v", len(data), err)
	}
	// What a lease can leave on the desktop.
	// (osascript to TextEdit waits at an Automation permission prompt.)
	e.mustHostel("run", "--", "mkdir -p ~/Applications && cp -R HostelSmoke.app ~/Applications/")
	agent("open", "TextEdit")
	// Once, without the retry: TextEdit corrects the text, so the read-back
	// check of agent-device can fail although the text is there.
	e.hostel("run", "--", `agent-device type "unsaved text" --platform macos`)
	e.mustHostel("release")

	if got := acquire(); got != room {
		t.Fatalf("second lease on room %s, want the warm room %s", got, room)
	}
	r := e.mustHostel("run", "--", `pgrep -x HostelSmoke || echo no-app; pgrep -x TextEdit || echo no-textedit; ls ~/Applications/HostelSmoke.app 2>/dev/null || echo no-copy`)
	if strings.Fields(r.stdout)[0] != "no-app" || !strings.Contains(r.stdout, "no-textedit") || !strings.Contains(r.stdout, "no-copy") {
		t.Fatalf("the desktop after the first lease:\n%s", r.stdout)
	}
	e.mustHostel("release")
	// The warm devbox stays for warm_running; the cleanup of
	// namespaceDevboxSmoke deletes it.
}
