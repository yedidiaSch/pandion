// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package flock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLockFD takes a non-blocking exclusive advisory lock (BSD flock semantics).
func tryLockFD(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func unlockFD(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}

// isWouldBlock reports the "already held" condition (EWOULDBLOCK/EAGAIN).
func isWouldBlock(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}
