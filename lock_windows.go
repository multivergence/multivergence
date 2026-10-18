// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package multivergence

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

const errorSharingViolation = syscall.Errno(32)

// lockFile opens path with no sharing at all, so no other process can open
// it until this one closes it or dies. It waits up to timeout for that.
func lockFile(path string, timeout time.Duration) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	wait := 100 * time.Microsecond
	for {
		h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil,
			syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
		if err == nil {
			return os.NewFile(uintptr(h), path), nil
		}
		if !errors.Is(err, errorSharingViolation) && !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: waited %v for %s", ErrLocked, timeout, path)
		}
		time.Sleep(wait)
		if wait < 4*time.Millisecond {
			wait *= 2
		}
	}
}

func unlockFile(f *os.File) { f.Close() }

func syncDir(string) {}

const fileStoresSupported = true
