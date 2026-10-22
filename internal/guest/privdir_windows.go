package guest

// checkPrivateDir: on Windows the directory is in the user's own
// %LOCALAPPDATA%, which other users cannot use.
func checkPrivateDir(dir string) error { return nil }
