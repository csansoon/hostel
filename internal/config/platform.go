package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Hostel OS names. Go's "darwin" maps to "macos".
const (
	OSLinux   = "linux"
	OSMacOS   = "macos"
	OSWindows = "windows"
)

// OSNames lists the OS names, which are also the OS platform names.
var OSNames = []string{OSLinux, OSMacOS, OSWindows}

// Platform script steps (design, section 8).
const (
	StepProvision  = "provision"
	StepPrepare    = "prepare"
	StepScreenshot = "screenshot"
	StepReset      = "reset"
	StepHealth     = "health"
)

var steps = []string{StepProvision, StepPrepare, StepScreenshot, StepReset, StepHealth}

// Platform kinds.
const (
	KindOS     = "os"
	KindDevice = "device"
)

// Platform is a platform that guests request: an OS platform or a device platform.
type Platform struct {
	Description    string              `toml:"description,omitempty"`
	Devices        []string            `toml:"devices,omitempty"`
	MaxDevices     int                 `toml:"max_devices,omitempty"`
	Requires       []string            `toml:"requires,omitempty"`
	Exclusive      bool                `toml:"exclusive,omitempty"`
	ScriptTimeouts map[string]Duration `toml:"script_timeouts,omitempty"`

	Name string `toml:"-"`
	Kind string `toml:"-"`
	// Dir is empty for an OS platform without a directory.
	Dir string `toml:"-"`
	// Scripts maps an OS and a step to the script file name in Dir.
	Scripts map[string]map[string]string `toml:"-"`
}

// devicePlatformFile and osPlatformFile are the two forms of platform.toml. An OS
// platform has no devices, so strict decoding reports them with their line.
type devicePlatformFile struct {
	Description    string              `toml:"description,omitempty"`
	Devices        []string            `toml:"devices,omitempty"`
	MaxDevices     int                 `toml:"max_devices,omitempty"`
	Requires       []string            `toml:"requires,omitempty"`
	Exclusive      bool                `toml:"exclusive,omitempty"`
	ScriptTimeouts map[string]Duration `toml:"script_timeouts,omitempty"`
}

type osPlatformFile struct {
	Description    string              `toml:"description,omitempty"`
	Requires       []string            `toml:"requires,omitempty"`
	Exclusive      bool                `toml:"exclusive,omitempty"`
	ScriptTimeouts map[string]Duration `toml:"script_timeouts,omitempty"`
}

// ScriptTimeout returns the deadline of a script step.
func (p *Platform) ScriptTimeout(step string) time.Duration {
	if d, ok := p.ScriptTimeouts[step]; ok {
		return d.D()
	}
	if step == StepProvision {
		return 20 * time.Minute
	}
	return 3 * time.Minute
}

// Script returns the script file name for a step on an OS, or "".
func (p *Platform) Script(osName, step string) string {
	return p.Scripts[osName][step]
}

// ScriptExt returns the script file extension for an OS.
func ScriptExt(osName string) string {
	if osName == OSWindows {
		return ".ps1"
	}
	return ".sh"
}

func isOS(name string) bool { return slices.Contains(OSNames, name) }

// IsOSName reports whether name is an OS (linux, macos, windows).
func IsOSName(name string) bool { return isOS(name) }

// IsPlatformFile reports whether a file name belongs in a platform:
// platform.toml, README.md, or a script (LoadPlatforms checks the script
// names).
func IsPlatformFile(name string) bool {
	if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return false
	}
	ext := filepath.Ext(name)
	return name == "platform.toml" || name == "README.md" || ext == ".sh" || ext == ".ps1"
}

// LoadPlatforms reads all platform directories in dir. The three OS platforms
// always exist, with or without a directory.
func LoadPlatforms(dir string) (map[string]*Platform, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("platforms_dir %s does not exist; create it (it can be empty)", dir)
		}
		return nil, fmt.Errorf("read platforms_dir: %w", err)
	}
	platforms := map[string]*Platform{}
	for _, name := range OSNames {
		platforms[name] = &Platform{Name: name, Kind: KindOS}
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p, err := loadPlatform(filepath.Join(dir, e.Name()), e.Name())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		platforms[p.Name] = p
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return platforms, nil
}

func loadPlatform(dir, name string) (*Platform, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%s: platform name %q must match %s", dir, name, nameRE)
	}
	p := &Platform{Name: name, Dir: dir, Kind: KindDevice, Scripts: map[string]map[string]string{}}
	if isOS(name) {
		p.Kind = KindOS
	}
	file := filepath.Join(dir, "platform.toml")
	_, statErr := os.Stat(file)
	switch {
	case statErr == nil && p.Kind == KindOS:
		var f osPlatformFile
		if _, err := decodeFile(file, &f); err != nil {
			return nil, err
		}
		p.Description, p.Requires, p.Exclusive, p.ScriptTimeouts = f.Description, f.Requires, f.Exclusive, f.ScriptTimeouts
	case statErr == nil:
		var f devicePlatformFile
		if _, err := decodeFile(file, &f); err != nil {
			return nil, err
		}
		p.Description, p.Devices, p.MaxDevices, p.Requires, p.Exclusive, p.ScriptTimeouts =
			f.Description, f.Devices, f.MaxDevices, f.Requires, f.Exclusive, f.ScriptTimeouts
	case errors.Is(statErr, os.ErrNotExist) && p.Kind == KindOS:
	case errors.Is(statErr, os.ErrNotExist):
		return nil, fmt.Errorf("%s: missing platform.toml (a device platform needs it)", dir)
	default:
		return nil, fmt.Errorf("%s: %w", file, statErr)
	}

	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{file}, args...)...))
	}
	if p.Kind == KindDevice {
		if len(p.Devices) == 0 {
			fail("devices: a device platform needs at least one device")
		}
		seen := map[string]bool{}
		for i, d := range p.Devices {
			if !nameRE.MatchString(d) {
				fail("devices[%d]: device name %q must match %s", i, d, nameRE)
			}
			if seen[d] {
				fail("devices[%d]: duplicate device %q", i, d)
			}
			seen[d] = true
		}
		if p.MaxDevices == 0 {
			p.MaxDevices = 1
		}
		if p.MaxDevices < 0 || (len(p.Devices) > 0 && p.MaxDevices > len(p.Devices)) {
			fail("max_devices: must be between 1 and the number of devices (%d)", len(p.Devices))
		}
	}
	for i, l := range p.Requires {
		if l == "" {
			fail("requires[%d]: empty label", i)
		}
	}
	for step := range p.ScriptTimeouts {
		if !slices.Contains(steps, step) {
			fail("script_timeouts.%s: unknown step; steps are %s", step, strings.Join(steps, ", "))
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read platform directory: %w", err)
	}
	for _, e := range entries {
		n := e.Name()
		ext := filepath.Ext(n)
		if e.IsDir() || (ext != ".sh" && ext != ".ps1") {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(n, ext), ".")
		if len(parts) != 2 || !slices.Contains(steps, parts[0]) || !isOS(parts[1]) {
			errs = append(errs, fmt.Errorf("%s: script %q must be named <step>.<os>%s; steps are %s; OSes are %s",
				dir, n, ext, strings.Join(steps, ", "), strings.Join(OSNames, ", ")))
			continue
		}
		step, scriptOS := parts[0], parts[1]
		if ext != ScriptExt(scriptOS) {
			errs = append(errs, fmt.Errorf("%s: script %q: %s scripts use %s", dir, n, scriptOS, ScriptExt(scriptOS)))
			continue
		}
		if p.Kind == KindOS && scriptOS != name {
			errs = append(errs, fmt.Errorf("%s: script %q: the %s platform can only have %s scripts", dir, n, name, name))
			continue
		}
		if p.Scripts[scriptOS] == nil {
			p.Scripts[scriptOS] = map[string]string{}
		}
		p.Scripts[scriptOS][step] = n
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return p, nil
}

// PlatformNames returns the platform names sorted, device platforms first.
func PlatformNames(platforms map[string]*Platform) []string {
	names := make([]string, 0, len(platforms))
	for n := range platforms {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := platforms[names[i]], platforms[names[j]]
		if a.Kind != b.Kind {
			return a.Kind == KindDevice
		}
		return a.Name < b.Name
	})
	return names
}
