// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/espoon-voltti/tfmux/internal/paths"
	"github.com/espoon-voltti/tfmux/internal/report"
	"github.com/espoon-voltti/tfmux/internal/state"
)

func runPending(args []string) error {
	asJSON, needsPlanOnly := false, false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "--needs-plan":
			needsPlanOnly = true
		default:
			return fmt.Errorf("pending: unknown flag %q", a)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	store, ignore, err := openStore()
	if err != nil {
		return err
	}
	repos, err := report.DiscoverWithGit(context.Background(), cfg)
	if err != nil {
		return err
	}
	rows, notes := report.Pending(store, ignore, repos, needsPlanOnly)
	for _, n := range notes {
		fmt.Fprintln(os.Stderr, "tfmux:", n)
	}
	if asJSON {
		if rows == nil {
			rows = []report.PendingRow{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	report.PrintPending(os.Stdout, rows, time.Now())
	return nil
}

// openStore opens the state store and its ignore list.
func openStore() (*state.Store, state.Ignore, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return nil, nil, err
	}
	store := state.New(dir)
	ignore, err := store.LoadIgnore()
	if err != nil {
		return nil, nil, err
	}
	return store, ignore, nil
}
