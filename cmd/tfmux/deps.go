// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/espoon-voltti/tfmux/internal/report"
)

func runDeps(args []string) error {
	asJSON, reverse := false, false
	var filter string
	for _, a := range args {
		switch {
		case a == "--json":
			asJSON = true
		case a == "--reverse" || a == "-r":
			reverse = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("deps: unknown flag %q", a)
		case filter != "":
			return fmt.Errorf("deps: only one filter allowed")
		default:
			filter = a
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	_, ignore, err := openStore()
	if err != nil {
		return err
	}
	repos, err := report.DiscoverWithGit(context.Background(), cfg)
	if err != nil {
		return err
	}
	mods, ix, behind := report.Deps(ignore, repos)
	for _, dm := range mods {
		for _, e := range dm.Deps.Errors {
			fmt.Fprintf(os.Stderr, "tfmux: %s/%s: %s\n", dm.Repo, dm.Module, e)
		}
	}
	switch {
	case asJSON:
		if mods == nil {
			mods = []report.DepsModule{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(mods)
	case reverse:
		report.PrintDepsReverse(os.Stdout, ix, behind, filter)
	default:
		report.PrintDepsForward(os.Stdout, mods, behind, filter)
	}
	return nil
}
