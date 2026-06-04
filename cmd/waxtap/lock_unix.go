//go:build unix

package main

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock (flock LOCK_EX), blocking until it is
// available. The lock is released by unlockFile or when the file is closed.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
