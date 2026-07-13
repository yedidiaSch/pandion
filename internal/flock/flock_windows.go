// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package flock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFD takes a non-blocking exclusive lock on the whole file via LockFileEx
// with LOCKFILE_FAIL_IMMEDIATELY, mirroring the Unix flock non-blocking path.
func tryLockFD(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol)
}

func unlockFD(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}

// isWouldBlock reports the "already held" condition. LockFileEx with
// FAIL_IMMEDIATELY returns ERROR_LOCK_VIOLATION when the file is already locked.
func isWouldBlock(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING)
}
