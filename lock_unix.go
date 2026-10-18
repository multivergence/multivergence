// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package multivergence

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockFile takes an exclusive flock on path, waiting up to timeout for
// another process to let go of it. The kernel drops the lock when the
// process dies, however it dies.
func lockFile(path string, timeout time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	wait := 100 * time.Microsecond
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("%w: waited %v for %s", ErrLocked, timeout, path)
		}
		time.Sleep(wait)
		if wait < 4*time.Millisecond {
			wait *= 2
		}
	}
}

func unlockFile(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}

// syncDir makes a new or renamed file's directory entry durable.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

const fileStoresSupported = true
