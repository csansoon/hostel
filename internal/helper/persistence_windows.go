package helper

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// persistenceKeys are the keys of the machine user's registry (HKCU) whose
// values start programs or change the environment of later processes
// (m11w-implementation.md, section 6). Only their values, not their subkeys.
var persistenceKeys = []string{
	`Software\Microsoft\Windows\CurrentVersion\Run`,
	`Software\Microsoft\Windows\CurrentVersion\RunOnce`,
	`Software\Microsoft\Windows\CurrentVersion\Policies\Explorer\Run`,
	`Software\Microsoft\Windows NT\CurrentVersion\Windows`,
	`Software\Microsoft\Windows NT\CurrentVersion\Winlogon`,
	`Software\Microsoft\Command Processor`,
	`Environment`,
	// Where the Startup and Documents folders are: a guest can move them
	// out of the audited places. Not "Shell Folders": Explorer writes that
	// cache itself.
	`Software\Microsoft\Windows\CurrentVersion\Explorer\User Shell Folders`,
}

var userIDRE = regexp.MustCompile(`<UserId>([^<]*)</UserId>`)

var procRegSetValueEx = windows.NewLazySystemDLL("advapi32.dll").NewProc("RegSetValueExW")

type regValue struct {
	Type uint32 `json:"type"`
	Data []byte `json:"data"`
}

// snapshotPlatform copies the values of persistenceKeys and the machine
// user's scheduled tasks (as XML).
func snapshotPlatform(base string) error {
	values := map[string]map[string]regValue{}
	for _, key := range persistenceKeys {
		v, err := readRegValues(key)
		if err != nil {
			return fmt.Errorf("HKCU\\%s: %w", key, err)
		}
		values[key] = v
	}
	data, err := json.Marshal(values)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(base, "registry.json"), data, 0o600); err != nil {
		return err
	}
	tasks, err := userTasks()
	if err != nil {
		return err
	}
	if data, err = json.Marshal(tasks); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(base, "tasks.json"), data, 0o600)
}

// readRegValues returns the values of an HKCU key; a missing key has none
// (nil).
func readRegValues(key string) (map[string]regValue, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer k.Close()
	names, err := k.ReadValueNames(0)
	if err != nil {
		return nil, err
	}
	out := map[string]regValue{}
	for _, n := range names {
		size, _, err := k.GetValue(n, nil)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size)
		_, typ, err := k.GetValue(n, buf)
		if err != nil {
			return nil, err
		}
		out[n] = regValue{Type: typ, Data: buf}
	}
	return out, nil
}

// restorePlatform brings the registry values and the scheduled tasks back to
// the copy, and returns the changed items.
func restorePlatform(base string) ([]string, error) {
	var items, failed []string
	data, err := os.ReadFile(filepath.Join(base, "registry.json"))
	if err != nil {
		return nil, err
	}
	var want map[string]map[string]regValue
	if err := json.Unmarshal(data, &want); err != nil {
		return nil, err
	}
	for _, key := range persistenceKeys {
		changed, err := restoreRegValues(key, want[key])
		for _, n := range changed {
			items = append(items, `HKCU\`+key+`\`+n)
		}
		if err != nil {
			failed = append(failed, fmt.Sprintf("HKCU\\%s: %v", key, err))
		}
	}
	changed, err := restoreTasks(base)
	items = append(items, changed...)
	if err != nil {
		failed = append(failed, err.Error())
	}
	if len(failed) > 0 {
		return items, errors.New(strings.Join(failed, "; "))
	}
	return items, nil
}

func restoreRegValues(key string, want map[string]regValue) ([]string, error) {
	cur, err := readRegValues(key)
	if err != nil {
		return nil, err
	}
	var changed []string
	for n, v := range cur {
		if w, ok := want[n]; !ok || w.Type != v.Type || string(w.Data) != string(v.Data) {
			changed = append(changed, n)
		}
	}
	for n := range want {
		if _, ok := cur[n]; !ok {
			changed = append(changed, n)
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	sort.Strings(changed)
	k, _, err := registry.CreateKey(registry.CURRENT_USER, key, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return nil, err
	}
	defer k.Close()
	for _, n := range changed {
		w, ok := want[n]
		if !ok {
			if err := k.DeleteValue(n); err != nil && !errors.Is(err, registry.ErrNotExist) {
				return changed, err
			}
			continue
		}
		if err := setRegValue(k, n, w); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// setRegValue writes a value with its type and bytes as they were.
func setRegValue(k registry.Key, name string, v regValue) error {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var data *byte
	if len(v.Data) > 0 {
		data = &v.Data[0]
	}
	if r, _, _ := procRegSetValueEx.Call(uintptr(k), uintptr(unsafe.Pointer(p)), 0, uintptr(v.Type),
		uintptr(unsafe.Pointer(data)), uintptr(len(v.Data))); r != 0 {
		return windows.Errno(r)
	}
	return nil
}

// userTasks returns the scheduled tasks of the machine user (a UserId of the
// task that names its SID or its account, in any spelling), by path, with
// their XML. schtasks.exe, not
// Get-ScheduledTask: CIM refuses the machine user over SSH (checked on
// Windows 11), and its XML does not depend on the system language.
func userTasks() (map[string]string, error) {
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		return nil, err
	}
	me := u.User.Sid
	known := map[string]bool{}
	isMe := func(id string) bool {
		id = strings.TrimSpace(id)
		if v, ok := known[id]; ok {
			return v
		}
		sid, err := windows.StringToSid(id)
		if err != nil {
			sid, _, _, err = windows.LookupSID("", strings.TrimPrefix(id, `.\`))
		}
		known[id] = err == nil && sid.Equals(me)
		return known[id]
	}
	out, err := schtasks("/query", "/xml", "ONE")
	if err != nil {
		return nil, err
	}
	tasks := map[string]string{}
	var path string
	var body []string
	flush := func() {
		x := strings.Join(body, "\n")
		if path != "" && slices.ContainsFunc(userIDRE.FindAllStringSubmatch(x, -1), func(m []string) bool { return isMe(m[1]) }) {
			tasks[path] = x
		}
		path, body = "", nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "<!-- \\") && strings.HasSuffix(t, " -->"):
			flush()
			path = strings.TrimSuffix(strings.TrimPrefix(t, "<!-- "), " -->")
		case t == "</Tasks>":
			flush()
		case path != "":
			body = append(body, line)
		}
	}
	return tasks, nil
}

// restoreTasks removes tasks of the machine user that are not in the copy and
// creates changed or removed ones again from their XML.
func restoreTasks(base string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(base, "tasks.json"))
	if err != nil {
		return nil, err
	}
	var want map[string]string
	if err := json.Unmarshal(data, &want); err != nil {
		return nil, err
	}
	cur, err := userTasks()
	if err != nil {
		return nil, err
	}
	var changed []string
	for p, x := range cur {
		if w, ok := want[p]; !ok || w != x {
			changed = append(changed, p)
		}
	}
	for p := range want {
		if _, ok := cur[p]; !ok {
			changed = append(changed, p)
		}
	}
	slices.Sort(changed)
	changed = slices.Compact(changed)
	var items, failed []string
	for _, p := range changed {
		var err error
		if x, ok := want[p]; ok {
			err = createTask(base, p, x)
		} else {
			_, err = schtasks("/delete", "/tn", p, "/f")
		}
		if err != nil {
			failed = append(failed, fmt.Sprintf("scheduled task %s: %v", p, err))
			continue
		}
		items = append(items, "scheduled task "+p)
	}
	if len(failed) > 0 {
		return items, errors.New(strings.Join(failed, "; "))
	}
	return items, nil
}

// createTask creates (or replaces) the task at path from its XML, written as
// the UTF-16 document that the Task Scheduler reads.
func createTask(base, path, x string) error {
	doc := "\ufeff<?xml version=\"1.0\" encoding=\"UTF-16\"?>\r\n" + strings.ReplaceAll(x, "\n", "\r\n")
	u := utf16.Encode([]rune(doc))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	f := filepath.Join(base, "task.xml")
	if err := os.WriteFile(f, b, 0o600); err != nil {
		return err
	}
	defer os.Remove(f)
	_, err := schtasks("/create", "/tn", path, "/xml", f, "/f")
	return err
}

func schtasks(args ...string) (string, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, filepath.Join(sys, "schtasks.exe"), args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("schtasks %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}
