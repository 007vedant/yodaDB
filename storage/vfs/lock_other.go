//go:build !darwin && !linux

package vfs

import (
	"errors"
	"os"
)

var errLockUnsupported = errors.New("advisory file locking is unsupported on this platform")

func lockFile(*os.File, LockMode) error { return errLockUnsupported }

func unlockFile(*os.File) error { return errLockUnsupported }

func isLockConflict(error) bool { return false }
