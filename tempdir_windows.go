//go:build windows

package main

import "os"

// %TEMP% is per-user on Windows, so the root needs no uid in its name.
func tempRootName() string {
	return "baton"
}

func ownedByCurrentUser(info os.FileInfo) bool {
	return true
}
