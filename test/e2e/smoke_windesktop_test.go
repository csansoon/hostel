//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/protocol"
)

// smokeWPF is a small WPF app: a text field, a label that counts, and a button.
var smokeWPF = map[string]string{
	"HostelSmoke.csproj": `<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <OutputType>WinExe</OutputType>
    <TargetFramework>net10.0-windows</TargetFramework>
    <UseWPF>true</UseWPF>
  </PropertyGroup>
</Project>
`,
	"App.xaml": `<Application x:Class="HostelSmoke.App" xmlns="http://schemas.microsoft.com/winfx/2006/xaml/presentation"
             xmlns:x="http://schemas.microsoft.com/winfx/2006/xaml" StartupUri="MainWindow.xaml"/>
`,
	"App.xaml.cs": "namespace HostelSmoke; public partial class App : System.Windows.Application { }\n",
	"MainWindow.xaml": `<Window x:Class="HostelSmoke.MainWindow" xmlns="http://schemas.microsoft.com/winfx/2006/xaml/presentation"
        xmlns:x="http://schemas.microsoft.com/winfx/2006/xaml" Title="Hostel Smoke" Width="360" Height="200"
        WindowStartupLocation="CenterScreen" Topmost="True">
  <StackPanel Margin="10">
    <TextBox x:Name="name" AutomationProperties.AutomationId="name"/>
    <TextBlock x:Name="count" AutomationProperties.AutomationId="count" Text="Count: 0" Margin="0,10"/>
    <Button AutomationProperties.AutomationId="add" Content="Add one" Click="Add"/>
  </StackPanel>
</Window>
`,
	"MainWindow.xaml.cs": `namespace HostelSmoke;
public partial class MainWindow : System.Windows.Window {
    int n;
    public MainWindow() { InitializeComponent(); }
    void Add(object s, System.Windows.RoutedEventArgs e) { n++; count.Text = "Count: " + n; }
}
`,
}

// TestSmokeWindowsDesktop (M7) runs against a real Windows machine with a
// desktop (HOSTEL_SMOKE_WINDOWS, as TestSmokeWindows; the machine needs the
// Hostel desktop task, docs/static-machine-setup.md). The desktop must be
// connected, and nobody may use it during the test. HOSTEL_SMOKE_OUT
// (optional) is a directory for the screenshot.
//
// A desktop lease builds a small WPF app, starts it on the desktop, reads and
// drives it with winapp ui (UI Automation, real clicks and keys), and takes a
// desktop screenshot. A second lease finds no process of the app.
func TestSmokeWindowsDesktop(t *testing.T) {
	e, s := windowsSmoke(t, true, "tools = [\"windows-desktop\"]\n", func(int) map[string]string { return smokeWPF })
	acquire := func() {
		start := time.Now()
		for {
			r := e.hostel("acquire", "windows-desktop", "--task", "windows desktop smoke test", "--wait", "1m")
			if r.code == 0 {
				t.Logf("lease ready after %s", time.Since(start).Round(time.Second))
				return
			}
			if r.code != 75 || time.Since(start) > 15*time.Minute {
				t.Fatalf("acquire: %s", r)
			}
		}
	}
	waitFree := func() {
		e.waitFor("the Windows machine free", 5*time.Minute, func() bool {
			r := s.machine("home/win")
			if r.State == protocol.MachineBroken {
				t.Fatalf("the Windows machine is broken: %s", r.Reason)
			}
			return r.State == protocol.MachineRunning && len(r.Rooms) == 1 && r.Rooms[0].State == protocol.RoomFree && r.Rooms[0].LeaseID == ""
		})
	}
	ui := func(args ...string) result {
		t.Helper()
		return e.mustHostel("run", "--", "winapp ui "+strings.Join(args, " ")+" -a HostelSmoke")
	}
	acquire()

	start := time.Now()
	e.mustHostel("run", "--", "dotnet build -c Release -o out -nologo -v q")
	t.Logf("app built in %s", time.Since(start).Round(time.Second))
	e.mustHostel("run", "--", `desktop --detach out\HostelSmoke.exe`)
	for deadline := time.Now().Add(time.Minute); ; {
		r := e.hostel("run", "--", "winapp ui status -a HostelSmoke")
		if r.code == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the app window: %s", r)
		}
		time.Sleep(time.Second)
	}
	if r := ui("inspect", "--interactive"); !strings.Contains(r.stdout, "add") || !strings.Contains(r.stdout, "name") {
		t.Fatalf("inspect: %s", r)
	}
	// UI Automation patterns, then real input.
	ui("invoke", "add")
	ui("invoke", "add")
	if r := ui("get-value", "count"); !strings.Contains(r.stdout, "Count: 2") {
		t.Fatalf("label after two invokes: %s", r)
	}
	ui("set-value", "name", `"typed by hostel"`)
	ui("click", "name")
	ui("send-keys", "+keys", "--via", "send-input")
	if r := ui("get-value", "name"); !strings.Contains(r.stdout, "typed by hostel+keys") {
		t.Fatalf("text field after set-value and keys: %s", r)
	}
	ui("click", "add")
	if r := ui("get-value", "count"); !strings.Contains(r.stdout, "Count: 3") {
		t.Fatalf("label after a click: %s", r)
	}
	shot := filepath.Join(t.TempDir(), "desktop.png")
	if dir := os.Getenv("HOSTEL_SMOKE_OUT"); dir != "" {
		shot = filepath.Join(dir, "smoke-windows-desktop.png")
	}
	e.mustHostel("screenshot", "-o", shot)
	if data, err := os.ReadFile(shot); err != nil || !bytes.HasPrefix(data, []byte("\x89PNG")) || len(data) < 10000 {
		t.Fatalf("desktop screenshot: %d bytes, %v", len(data), err)
	}
	e.mustHostel("release")
	waitFree()

	acquire()
	if r := e.mustHostel("run", "--", "@(Get-Process HostelSmoke -ErrorAction SilentlyContinue).Count"); strings.TrimSpace(r.stdout) != "0" {
		t.Fatalf("processes of the app after the first lease: %s", r.stdout)
	}
	e.mustHostel("release")
	waitFree()
}
