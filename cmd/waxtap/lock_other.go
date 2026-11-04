//go:build !unix

package main

import "os"

// Without flock, archive locking is limited to the in-process mutex.
// Cross-process locking on Windows would need LockFileEx.
func lockFile(*os.File) error   { return nil }
func unlockFile(*os.File) error { return nil }
