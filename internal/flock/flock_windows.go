// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build windows

package flock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockByteOffset is a reserved byte, far past the pid stamped at offset 0, that
// carries the exclusive lock. LockFileEx blocks reads of the locked region from
// other handles, so locking byte 0 (where the pid lives) would stop a contender
// from reading the holder's pid for the BusyError message. Locking a byte beyond
// the pid — it need not exist in the file — keeps the pid readable. Windows only
// (Unix flock is whole-fd advisory and never blocks reads).
const lockByteOffset = 1 << 20

func overlappedAt(off uint32) *windows.Overlapped {
	return &windows.Overlapped{Offset: off}
}

// tryLockFD takes a non-blocking exclusive lock on the reserved byte via
// LockFileEx with LOCKFILE_FAIL_IMMEDIATELY, mirroring the Unix non-blocking path.
func tryLockFD(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, overlappedAt(lockByteOffset))
}

func unlockFD(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlappedAt(lockByteOffset))
}

// isWouldBlock reports the "already held" condition. LockFileEx with
// FAIL_IMMEDIATELY returns ERROR_LOCK_VIOLATION when the file is already locked.
func isWouldBlock(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING)
}
