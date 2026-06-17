// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

//go:build unix

package runner

import (
	"context"
	"os"
	"syscall"
	"time"
)

// flockExclusive opens path and takes an exclusive advisory lock on it,
// polling (cancellably) until it's free. The returned file holds the lock;
// close it to release. Separate open file descriptions contend even within one
// process, so this serializes both across tfmux instances and — though the
// runner's busyModule check already prevents it — within one.
func flockExclusive(ctx context.Context, path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
