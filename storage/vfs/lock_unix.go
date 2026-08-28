//go:build darwin || linux

package vfs

import (
	"errors"
	"os"
	"syscall"
)

func lockFile(file *os.File, mode LockMode) error {
	operation := syscall.LOCK_SH | syscall.LOCK_NB
	if mode == LockExclusive {
		operation = syscall.LOCK_EX | syscall.LOCK_NB
	}
	return syscall.Flock(int(file.Fd()), operation)
}

func unlockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func isLockConflict(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
