//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

func tempRootName() string {
	return fmt.Sprintf("baton-%d", os.Getuid())
}

func ownedByCurrentUser(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
