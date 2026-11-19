//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/csansoon/hostel/internal/config"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/provider"
)

// TestSmokeAzure runs against a real Azure virtual machine (M12). It costs
// money (the machine runs for some minutes), so it runs only with
// HOSTEL_SMOKE_AZURE=1 ("make test-smoke-azure"), and:
//
//   - HOSTEL_SMOKE_AZURE_TENANT, HOSTEL_SMOKE_AZURE_CLIENT_ID: the application;
//   - HOSTEL_SMOKE_AZURE_SECRET_FILE: a file with its client secret;
//   - HOSTEL_SMOKE_AZURE_VM: the resource ID of the machine;
//   - HOSTEL_SMOKE_WINDOWS and HOSTEL_SMOKE_WINDOWS_KEY: the SSH address of
//     the machine when it runs, and the key that its machine user accepts
//     (as for TestSmokeWindows).
//
// The machine must be deallocated at the start, with no other Hostel server
// that uses it. The test starts it with a lease, and Hostel deallocates it
// after a short warm_running. The test deallocates it also when it fails.
func TestSmokeAzure(t *testing.T) {
	if os.Getenv("HOSTEL_SMOKE_AZURE") != "1" {
		t.Skip("set HOSTEL_SMOKE_AZURE=1 (it starts a real Azure machine; see the test comment)")
	}
	vars := map[string]string{}
	for _, k := range []string{"HOSTEL_SMOKE_AZURE_TENANT", "HOSTEL_SMOKE_AZURE_CLIENT_ID", "HOSTEL_SMOKE_AZURE_SECRET_FILE",
		"HOSTEL_SMOKE_AZURE_VM", "HOSTEL_SMOKE_WINDOWS", "HOSTEL_SMOKE_WINDOWS_KEY"} {
		if vars[k] = os.Getenv(k); vars[k] == "" {
			t.Fatalf("%s is not set", k)
		}
	}
	secret, err := os.ReadFile(vars["HOSTEL_SMOKE_AZURE_SECRET_FILE"])
	if err != nil {
		t.Fatalf("read HOSTEL_SMOKE_AZURE_SECRET_FILE: %v", err)
	}
	key, err := os.ReadFile(vars["HOSTEL_SMOKE_WINDOWS_KEY"])
	if err != nil {
		t.Fatalf("read HOSTEL_SMOKE_WINDOWS_KEY: %v", err)
	}
	pub, err := os.ReadFile(vars["HOSTEL_SMOKE_WINDOWS_KEY"] + ".pub")
	if err != nil {
		t.Fatalf("read the public key of HOSTEL_SMOKE_WINDOWS_KEY: %v", err)
	}
	dist, err := filepath.Abs("../../dist")
	if err != nil {
		t.Fatal(err)
	}

	// The power state at the start, and the deallocation at the end, with the
	// provider itself (also when the server of the test fails).
	e := newEnv(t)
	secretFile := filepath.Join(e.home, "azure-secret")
	e.writeFile(secretFile, string(secret))
	p := &config.Provider{Name: "cloud", Kind: config.ProviderAzure, Tenant: vars["HOSTEL_SMOKE_AZURE_TENANT"], ClientID: vars["HOSTEL_SMOKE_AZURE_CLIENT_ID"],
		TemplateOrder: []string{"win"}, Templates: map[string]*config.Template{"win": {Name: "win", VM: vars["HOSTEL_SMOKE_AZURE_VM"]}}}
	az := provider.NewAzure(p, secretFile, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	listed, err := az.List(ctx, "")
	cancel()
	if err != nil {
		t.Fatalf("read the power state: %v", err)
	}
	if len(listed) != 1 || listed[0].State != provider.PowerStopped {
		t.Fatalf("the machine must be deallocated at the start (another server can use it): %+v", listed)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if err := az.Stop(ctx, provider.MachineRef{Name: "win", Template: "win"}); err != nil {
			t.Errorf("deallocate the machine at the end: %v; deallocate it in the Azure portal", err)
		}
	})

	e.writeFile(filepath.Join(e.dataDir(), "server_ed25519"), string(key))
	e.writeFile(filepath.Join(e.dataDir(), "server_ed25519.pub"), string(pub))
	if err := os.Chmod(filepath.Join(e.dataDir(), "server_ed25519"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.init()
	header := strings.Replace(serverHeader(), fmt.Sprintf("helper_dir = %q", helperDir), fmt.Sprintf("helper_dir = %q", dist), 1)
	header = strings.Replace(header, `reconcile = "2s"`, `reconcile = "15s"`, 1)
	e.writeConfig(header + fmt.Sprintf(`
[providers.cloud]
kind = "azure"
tenant = %q
client_id = %q

[providers.cloud.machines.win]
vm = %q
address = %q
os = "windows"
warm_running = "1m"
`, p.Tenant, p.ClientID, p.Templates["win"].VM, vars["HOSTEL_SMOKE_WINDOWS"]))
	s := e.startServer(e.vars["HOSTEL_TOKEN"])
	saved := e.vars["HOSTEL_TOKEN"]
	e.vars["HOSTEL_TOKEN"] = e.admin
	if r := e.hostelIn(string(secret), "config", "providers", "login", "cloud"); r.code != 0 {
		t.Fatalf("providers login: %s", r)
	}
	e.vars["HOSTEL_TOKEN"] = saved
	s.waitMachine("cloud/win", protocol.MachineStopped, 2*time.Minute)

	e.useGuestToken()
	e.newRepo(map[string]string{"a.txt": "hello\n"})
	began := time.Now()
	if r := e.hostel("acquire", "--task", "azure smoke test", "--wait", "100s"); r.code != 0 {
		for i := 0; i < 5 && r.code == 75; i++ {
			r = e.hostel("acquire", "--task", "azure smoke test", "--wait", "100s")
		}
		if r.code != 0 {
			t.Fatalf("acquire: %s", r)
		}
	}
	t.Logf("the lease was ready %s after acquire (start of the machine and provisioning)", time.Since(began).Round(time.Second))
	if r := e.mustHostel("run", "--", "Get-Content a.txt"); !strings.Contains(r.stdout, "hello") {
		t.Fatalf("run: %s", r)
	}
	e.mustHostel("release")
	s.waitMachine("cloud/win", protocol.MachineStopped, 15*time.Minute)
	ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
	listed, err = az.List(ctx, "")
	cancel()
	if err != nil || len(listed) != 1 || listed[0].State != provider.PowerStopped {
		t.Fatalf("Azure power state after warm_running: %+v (err %v), want deallocated", listed, err)
	}
}
