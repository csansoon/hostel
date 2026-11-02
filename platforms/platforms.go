// Package platforms embeds the default platforms (ios, android, macos,
// windows) in the binary. A server uses each default until an admin replaces
// or removes it (multiplayer.md, section 7).
package platforms

import (
	"embed"
	"io/fs"
)

//go:embed android ios macos windows
var files embed.FS

// Defaults returns the files of each default platform: name → file → content.
func Defaults() map[string]map[string][]byte {
	out := map[string]map[string][]byte{}
	dirs, _ := fs.ReadDir(files, ".")
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		entries, _ := fs.ReadDir(files, d.Name())
		out[d.Name()] = map[string][]byte{}
		for _, e := range entries {
			data, _ := fs.ReadFile(files, d.Name()+"/"+e.Name())
			out[d.Name()][e.Name()] = data
		}
	}
	return out
}
