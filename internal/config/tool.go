package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Hostel OS names. Go's "darwin" maps to "macos".
const (
	OSLinux   = "linux"
	OSMacOS   = "macos"
	OSWindows = "windows"
)

// OSNames lists the OS names.
var OSNames = []string{OSLinux, OSMacOS, OSWindows}

// Tool script steps (design, section 8, "Tool directories").
const (
	StepInstall    = "install"
	StepCheck      = "check"
	StepStart      = "start"
	StepScreenshot = "screenshot"
	StepStop       = "stop"
)

var steps = []string{StepInstall, StepCheck, StepStart, StepScreenshot, StepStop}

// screenSteps run for each lease; only screen tools have them.
var screenSteps = []string{StepStart, StepScreenshot, StepStop}

// Files of a tool directory besides its scripts.
const (
	ToolFile  = "tool.toml"
	GuideFile = "GUIDE.md"
)

// Tool is a defined tool: a directory with tool.toml, scripts for each OS, and
// GUIDE.md. A tool without a definition is a mise tool.
type Tool struct {
	Description    string              `toml:"description"`
	Needs          []string            `toml:"needs,omitempty"`
	Screen         bool                `toml:"screen,omitempty"`
	Options        []string            `toml:"options,omitempty"`
	MaxOptions     int                 `toml:"max_options,omitempty"`
	WholeMachine   bool                `toml:"whole_machine,omitempty"`
	Env            map[string]string   `toml:"env,omitempty"`
	Path           []string            `toml:"path,omitempty"`
	ScriptTimeouts map[string]Duration `toml:"script_timeouts,omitempty"`

	Name string `toml:"-"`
	// Dir is the directory with the tool's files on the server (empty when
	// the files are not on disk, for example in "hostel admin config check").
	Dir string `toml:"-"`
	// Scripts maps an OS and a step to the script file name.
	Scripts map[string]map[string]string `toml:"-"`
	Guide   string                       `toml:"-"`
}

// ScriptTimeout returns the deadline of a script step.
func (t *Tool) ScriptTimeout(step string) time.Duration {
	if d, ok := t.ScriptTimeouts[step]; ok {
		return d.D()
	}
	if step == StepInstall {
		return 20 * time.Minute
	}
	return 3 * time.Minute
}

// Script returns the script file name for a step on an OS, or "".
func (t *Tool) Script(osName, step string) string {
	return t.Scripts[osName][step]
}

// AvailableOn reports whether the tool works on an OS: it has a script for the
// OS, or no scripts at all (a tool that only sets env and path).
func (t *Tool) AvailableOn(osName string) bool {
	return len(t.Scripts) == 0 || len(t.Scripts[osName]) > 0
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

// IsToolFile reports whether a file name belongs in a tool directory:
// tool.toml, GUIDE.md, or a script (ParseTool checks the script names).
func IsToolFile(name string) bool {
	if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return false
	}
	ext := filepath.Ext(name)
	return name == ToolFile || name == GuideFile || ext == ".sh" || ext == ".ps1"
}

// oldToolKeys are the keys of platform.toml before M10e, with the new form.
var oldToolKeys = map[string]string{
	"devices":     "options (and screen = true)",
	"max_devices": "max_options",
	"exclusive":   "whole_machine",
	"requires":    "nothing: labels are gone; list the tool only on the machines that have what it needs",
}

var placeholderToolRE = regexp.MustCompile(`\{([^{}]*)\}`)

// ParseTool reads and validates one tool from its files (file name →
// content). Errors name the tool.
func ParseTool(name string, files map[string]string) (*Tool, error) {
	where := "tool " + name
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%s: the name must match %s", where, nameRE)
	}
	t := &Tool{Name: name, Scripts: map[string]map[string]string{}, Guide: files[GuideFile]}
	file := where + "/" + ToolFile
	data, ok := files[ToolFile]
	if !ok {
		if _, old := files["platform.toml"]; old {
			return nil, fmt.Errorf("%s: platform.toml is the form before M10e; rename it to %s and use the keys of a tool (docs/design.md, section 8)", where, ToolFile)
		}
		return nil, fmt.Errorf("%s: missing %s", where, ToolFile)
	}
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{file}, args...)...))
	}
	var raw map[string]any
	if err := toml.Unmarshal([]byte(data), &raw); err == nil {
		for _, k := range sortedKeys(oldToolKeys) {
			if _, ok := raw[k]; ok {
				fail("%s: a key of platform.toml before M10e; use %s", k, oldToolKeys[k])
			}
		}
		if len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
	}
	if err := decodeData(file, []byte(data), t); err != nil {
		return nil, err
	}
	if t.Description == "" {
		fail("description: required, for example \"Android emulator: one clean emulator for each lease\"")
	}
	seen := map[string]bool{}
	for i, o := range t.Options {
		if !nameRE.MatchString(o) {
			fail("options[%d]: option %q must match %s", i, o, nameRE)
		}
		if seen[o] {
			fail("options[%d]: duplicate option %q", i, o)
		}
		seen[o] = true
	}
	if len(t.Options) > 0 && !t.Screen {
		fail("options: only a screen tool has options (the option of a software tool is its version)")
	}
	switch {
	case t.MaxOptions == 0 && len(t.Options) > 0:
		t.MaxOptions = 1
	case t.MaxOptions != 0 && (t.MaxOptions < 1 || t.MaxOptions > len(t.Options)):
		fail("max_options: must be between 1 and the number of options (%d)", len(t.Options))
	}
	for i, n := range t.Needs {
		if !nameRE.MatchString(n) {
			fail("needs[%d]: tool name %q must match %s", i, n, nameRE)
		}
		if n == name {
			fail("needs[%d]: a tool cannot need itself", i)
		}
	}
	for k, v := range t.Env {
		if k == "" {
			fail("env: empty variable name")
		}
		checkToolPlaceholders(fail, "env."+k, v)
	}
	for i, p := range t.Path {
		if p == "" {
			fail("path[%d]: empty entry", i)
		}
		checkToolPlaceholders(fail, fmt.Sprintf("path[%d]", i), p)
	}
	for step := range t.ScriptTimeouts {
		if !slices.Contains(steps, step) {
			fail("script_timeouts.%s: unknown step; steps are %s", step, strings.Join(steps, ", "))
		}
	}

	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !IsToolFile(n) {
			errs = append(errs, fmt.Errorf("%s: %q is not a tool file; a tool has %s, %s, and scripts named <step>.<os>.sh or .ps1", where, n, ToolFile, GuideFile))
			continue
		}
		ext := filepath.Ext(n)
		if ext != ".sh" && ext != ".ps1" {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(n, ext), ".")
		if len(parts) != 2 || !slices.Contains(steps, parts[0]) || !isOS(parts[1]) {
			errs = append(errs, fmt.Errorf("%s: script %q must be named <step>.<os>%s; steps are %s; OSes are %s",
				where, n, ext, strings.Join(steps, ", "), strings.Join(OSNames, ", ")))
			continue
		}
		step, scriptOS := parts[0], parts[1]
		if ext != ScriptExt(scriptOS) {
			errs = append(errs, fmt.Errorf("%s: script %q: %s scripts use %s", where, n, scriptOS, ScriptExt(scriptOS)))
			continue
		}
		if !t.Screen && slices.Contains(screenSteps, step) {
			errs = append(errs, fmt.Errorf("%s: script %q: only a screen tool (screen = true) has %s scripts", where, n, strings.Join(screenSteps, ", ")))
			continue
		}
		if t.Scripts[scriptOS] == nil {
			t.Scripts[scriptOS] = map[string]string{}
		}
		t.Scripts[scriptOS][step] = n
	}
	if t.Screen {
		if len(t.Scripts) == 0 {
			errs = append(errs, fmt.Errorf("%s: a screen tool needs a start script for each OS that it works on, for example %s",
				where, StepStart+"."+OSLinux+ScriptExt(OSLinux)))
		}
		for _, osName := range OSNames {
			if len(t.Scripts[osName]) > 0 && t.Scripts[osName][StepStart] == "" {
				errs = append(errs, fmt.Errorf("%s: a screen tool needs %s, because it has other %s scripts", where, StepStart+"."+osName+ScriptExt(osName), osName))
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return t, nil
}

func checkToolPlaceholders(fail func(string, ...any), key, v string) {
	for _, m := range placeholderToolRE.FindAllStringSubmatch(v, -1) {
		if m[1] != "cache" && m[1] != "home" {
			fail("%s: unknown placeholder %s; use {cache} or {home}", key, m[0])
		}
	}
}

// ParseTools reads and validates a set of tools: name → file → content. It
// also checks that the needs of the defined tools make no loop.
func ParseTools(set map[string]map[string]string) (map[string]*Tool, error) {
	out := map[string]*Tool{}
	var errs []error
	for _, name := range sortedKeys(set) {
		t, err := ParseTool(name, set[name])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out[name] = t
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	for _, name := range sortedKeys(out) {
		if loop := needsLoop(out, name, nil); loop != nil {
			return nil, fmt.Errorf("tool %s: needs make a loop: %s", name, strings.Join(loop, " → "))
		}
	}
	return out, nil
}

// needsLoop returns a path of needs from name back to a tool on the path, or
// nil.
func needsLoop(tools map[string]*Tool, name string, path []string) []string {
	if slices.Contains(path, name) {
		return append(path, name)
	}
	t := tools[name]
	if t == nil {
		return nil
	}
	for _, n := range t.Needs {
		if loop := needsLoop(tools, n, append(slices.Clone(path), name)); loop != nil {
			return loop
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ToolNames returns the names of the tools sorted.
func ToolNames(tools map[string]*Tool) []string { return sortedKeys(tools) }

// ToolEntry is an entry of a tools list: "name" or "name:option".
type ToolEntry struct {
	Name   string
	Option string
}

func (e ToolEntry) String() string {
	if e.Option == "" {
		return e.Name
	}
	return e.Name + ":" + e.Option
}

var optionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// ParseToolEntry parses "name" or "name:option".
func ParseToolEntry(s string) (ToolEntry, error) {
	name, option, hasOption := strings.Cut(s, ":")
	e := ToolEntry{Name: name, Option: option}
	if !nameRE.MatchString(name) {
		return e, fmt.Errorf("%q: the tool name must match %s (an entry is \"name\" or \"name:option\")", s, nameRE)
	}
	if hasOption && !optionRE.MatchString(option) {
		return e, fmt.Errorf("%q: the option must match %s", s, optionRE)
	}
	return e, nil
}

// MachineTool is a tool of a machine after the configuration is resolved.
type MachineTool struct {
	ToolEntry
	// Mise: the tool has no definition; mise installs it, and the option is
	// the version.
	Mise bool
	// Need: no entry of the machine lists it; a tool on the machine needs it.
	Need bool
}
