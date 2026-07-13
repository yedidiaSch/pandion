// SPDX-License-Identifier: AGPL-3.0-or-later

// Package flock is a tiny cross-process advisory lock over a lock file, used to
// serialize the mutating pandion commands (up/down/start/lockdown/reap) that
// touch a single cluster's on-disk state. The in-process sync.Mutex in the
// orchestrator only guards one process; this guards two concurrent invocations
// (e.g. `up` racing `reap`, or a double `up --id x`) from interleaving writes to
// ~/.pandion/state/<id>.json and the manifest.
//
// The lock is ADVISORY (it only excludes other flock callers) and NON-BLOCKING:
// TryLock fails immediately with *BusyError naming the holder's pid rather than
// waiting, so the user gets an actionable message instead of a silent hang.
package flock

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Lock is a held advisory lock. Release it with Unlock (safe to defer).
type Lock struct {
	f    *os.File
	path string
}

// BusyError reports that another process already holds the lock. Pid is the
// holder's process id (0 if it could not be read from the lock file).
type BusyError struct {
	Path string
	Pid  int
}

func (e *BusyError) Error() string {
	if e.Pid > 0 {
		return fmt.Sprintf("another pandion command is operating on this cluster (pid %d) — retry when it finishes", e.Pid)
	}
	return "another pandion command is operating on this cluster — retry when it finishes"
}

// IsBusy reports whether err is a lock-contention error.
func IsBusy(err error) bool {
	var be *BusyError
	return errors.As(err, &be)
}

// TryLock acquires the advisory lock at path without blocking. On success it
// records the caller's pid in the file (best-effort) and returns a *Lock. On
// contention it returns a *BusyError naming the current holder.
func TryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := tryLockFD(f); err != nil {
		// Contention (or a real error): read whoever wrote their pid, for the message.
		pid := readPid(f)
		f.Close()
		if isWouldBlock(err) {
			return nil, &BusyError{Path: path, Pid: pid}
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// We hold it: stamp our pid so a later contender can name us.
	_ = f.Truncate(0)
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err == nil {
		_ = f.Sync()
	}
	return &Lock{f: f, path: path}, nil
}

// Unlock releases the lock. The lock file is left in place (its presence is not
// the lock; the OS advisory lock is), so a stale file after a crash is harmless.
func (l *Lock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlockFD(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

func readPid(f *os.File) int {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}
