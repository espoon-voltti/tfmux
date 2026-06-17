// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

//go:build unix

package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/espoon-voltti/tfmux/internal/domain"
	"github.com/espoon-voltti/tfmux/internal/state"
	"github.com/espoon-voltti/tfmux/internal/tftest"
)

// A second acquire blocks while the first is held, then succeeds on release.
func TestFlockExclusiveSerializes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	f1, err := flockExclusive(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan struct{})
	go func() {
		f2, err := flockExclusive(context.Background(), path)
		if err == nil {
			f2.Close()
		}
		close(acquired)
	}()
	select {
	case <-acquired:
		t.Fatal("second lock acquired while the first was held")
	case <-time.After(400 * time.Millisecond):
	}
	f1.Close()
	select {
	case <-acquired:
	case <-time.After(3 * time.Second):
		t.Fatal("second lock not acquired after release")
	}
}

// A blocked acquire returns promptly when its context is canceled (so a queued
// task waiting on another instance can still be canceled).
func TestFlockExclusiveCancelable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	held, err := flockExclusive(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, e := flockExclusive(ctx, path)
		errc <- e
	}()
	cancel()
	select {
	case e := <-errc:
		if e == nil {
			t.Fatal("expected a cancellation error, got nil (lock acquired despite the held lock?)")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked acquire did not return after cancel")
	}
}

// Two runners sharing the state dir (standing in for two tfmux instances) must
// not run terraform in the same module dir at the same time.
func TestModuleLockSerializesAcrossRunners(t *testing.T) {
	bin := tftest.Write(t, t.TempDir())
	logFile := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("TFMUX_FAKE_LOG", logFile)
	t.Setenv("TFMUX_FAKE_SLEEP", "1") // widen the window so overlap would show

	store := state.New(t.TempDir()) // shared store => shared module lock
	moduleDir := filepath.Join(t.TempDir(), "mod")
	if err := os.MkdirAll(filepath.Join(moduleDir, ".terraform"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkMod := func() *domain.Module {
		repo := &domain.Repo{Path: moduleDir, Name: "mod"}
		m := &domain.Module{Repo: repo, Path: moduleDir, RelPath: ".", TFBin: bin}
		repo.Modules = []*domain.Module{m}
		return m
	}

	rA := New(2, store, nil)
	rB := New(2, store, nil)
	if !rA.EnqueueEnumerate(mkMod()) || !rB.EnqueueEnumerate(mkMod()) {
		t.Fatal("enqueue refused")
	}
	waitTerminal(t, rA.Events, KindEnumerate, 1)
	waitTerminal(t, rB.Events, KindEnumerate, 1)

	// The fake brackets each terraform call with start/end log lines. Had the
	// instances run concurrently the pairs would interleave (depth > 1); the
	// lock forces start,end,start,end.
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	depth := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		switch {
		case strings.HasPrefix(line, "start"):
			depth++
			if depth > 1 {
				t.Fatalf("overlapping terraform runs in the same module dir:\n%s", data)
			}
		case strings.HasPrefix(line, "end"):
			depth--
		}
	}
}
