package room

import (
	"os"
	"runtime"

	"github.com/csansoon/hostel/internal/hashes"
	"github.com/csansoon/hostel/internal/protocol"
	"github.com/csansoon/hostel/internal/version"
)

func (h *helper) info() (protocol.HelperInfo, error) {
	out := protocol.HelperInfo{OS: h.os, Arch: runtime.GOARCH, Version: version.Version,
		Hashes: protocol.HelperHashes{Tools: map[string]string{}}}
	var err error
	for _, item := range []struct {
		dst  *string
		path string
	}{
		{&out.Hashes.Helper, h.exe},
		{&out.Hashes.Mise, h.misePath()},
		{&out.Hashes.MiseConfig, h.miseConfigPath()},
		{&out.Hashes.RoomEnv, h.roomEnvPath()},
	} {
		if *item.dst, err = hashes.File(item.path); err != nil {
			return out, err
		}
	}
	des, err := os.ReadDir(h.toolsDir())
	if err != nil && !os.IsNotExist(err) {
		return out, err
	}
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		entries, err := hashes.DirEntries(h.toolDir(de.Name()), nil, runtime.GOOS != "windows")
		if err != nil {
			return out, err
		}
		out.Hashes.Tools[de.Name()] = hashes.Dir(entries)
	}
	return out, nil
}
