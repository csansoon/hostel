//go:build !windows

package helper

func snapshotPlatform(string) error { return nil }

func restorePlatform(string) ([]string, error) { return nil, nil }
