// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

//go:build !unix

package runner

import (
	"context"
	"os"
)

// flockExclusive is a no-op on platforms without flock(2): it opens the file
// so the close-to-release contract still holds, but provides no cross-process
// exclusion. tfmux targets unix; this only keeps other platforms compiling.
func flockExclusive(_ context.Context, path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}
