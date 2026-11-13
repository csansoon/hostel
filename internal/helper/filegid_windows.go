package helper

import "io/fs"

func fileGID(fs.FileInfo) (int, bool) { return 0, false }
