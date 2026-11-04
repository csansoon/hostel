// Package update finds, verifies, and installs Hostel releases (M10d,
// m10d-implementation.md): the CLI, the server, and the binary of a machine
// that joined.
package update

import (
	"regexp"
	"strconv"
)

// Version is a release version vX.Y.Z; After is true for a git describe
// version (vX.Y.Z-N-gHASH[-dirty]), which is after vX.Y.Z.
type Version struct {
	Major, Minor, Patch int
	After               bool
}

var (
	tagRE      = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)
	describeRE = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)-\d+-g[0-9a-f]+(-dirty)?$|^v(\d+)\.(\d+)\.(\d+)-dirty$`)
)

// ParseTag parses a release tag (vX.Y.Z only).
func ParseTag(s string) (Version, bool) {
	m := tagRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, false
	}
	return version(m[1], m[2], m[3], false), true
}

// Parse parses a build version: a tag or a git describe version. Other
// versions ("dev", a commit hash) are unknown.
func Parse(s string) (Version, bool) {
	if v, ok := ParseTag(s); ok {
		return v, true
	}
	m := describeRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, false
	}
	if m[1] != "" {
		return version(m[1], m[2], m[3], true), true
	}
	return version(m[5], m[6], m[7], true), true
}

func version(a, b, c string, after bool) Version {
	x, _ := strconv.Atoi(a)
	y, _ := strconv.Atoi(b)
	z, _ := strconv.Atoi(c)
	return Version{x, y, z, after}
}

// Less reports v < w.
func (v Version) Less(w Version) bool {
	if v.Major != w.Major {
		return v.Major < w.Major
	}
	if v.Minor != w.Minor {
		return v.Minor < w.Minor
	}
	if v.Patch != w.Patch {
		return v.Patch < w.Patch
	}
	return !v.After && w.After
}

// Newer reports that the release tag is newer than the build version have.
// An unknown version is never older (it gets no notice).
func Newer(tag, have string) bool {
	t, ok := ParseTag(tag)
	if !ok {
		return false
	}
	h, ok := Parse(have)
	return ok && h.Less(t)
}

// Older reports that the build version have is known and older than the
// build version want (both known).
func Older(have, want string) bool {
	h, ok := Parse(have)
	if !ok {
		return false
	}
	w, ok := Parse(want)
	return ok && h.Less(w)
}
