// Package version holds the build version of the hostel binary.
package version

// Version is set at build time with
// -ldflags "-X github.com/csansoon/hostel/internal/version.Version=<git describe>".
var Version = "dev"
