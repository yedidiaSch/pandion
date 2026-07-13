// SPDX-License-Identifier: AGPL-3.0-or-later

package flock

import (
	"os"
	"path/filepath"
	"testing"
)

// TestTryLockContention asserts a second acquire of a held lock fails with a
// *BusyError naming the holder pid, and that releasing lets the next caller in.
func TestTryLockContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster.lock")

	l1, err := TryLock(path)
	if err != nil {
		t.Fatalf("first TryLock: %v", err)
	}

	_, err = TryLock(path)
	if !IsBusy(err) {
		t.Fatalf("second TryLock should be busy, got %v", err)
	}
	var be *BusyError
	if !asBusy(err, &be) || be.Pid != os.Getpid() {
		t.Fatalf("BusyError should name our pid %d, got %+v (err=%v)", os.Getpid(), be, err)
	}

	if err := l1.Unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	l2, err := TryLock(path)
	if err != nil {
		t.Fatalf("TryLock after release should succeed, got %v", err)
	}
	_ = l2.Unlock()
}

// asBusy is a tiny errors.As shim kept local so the test reads without importing
// errors just for one call.
func asBusy(err error, target **BusyError) bool {
	if be, ok := err.(*BusyError); ok {
		*target = be
		return true
	}
	return false
}
