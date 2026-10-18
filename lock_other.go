// Copyright Multivergence.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package multivergence

import (
	"errors"
	"os"
	"time"
)

func lockFile(string, time.Duration) (*os.File, error) {
	return nil, errors.New("multivergence: file stores aren't supported on this platform; use New")
}

func unlockFile(f *os.File) {}

func syncDir(string) {}

const fileStoresSupported = false
