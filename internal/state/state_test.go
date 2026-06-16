// SPDX-FileCopyrightText: 2026 City of Espoo
//
// SPDX-License-Identifier: LGPL-2.1-or-later

package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunRecordRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	ec := 0
	now := time.Now().Truncate(time.Second)
	rec := &RunRecord{
		ModulePath:   "/work/iac/repo/envs/prod",
		Workspace:    "prod",
		PlanStarted:  now,
		PlanFinished: now.Add(time.Minute),
		PlanExitCode: 2,
		Summary:      ChangeSummary{Add: 2, Change: 1},
		GitHead:      "abc123",
		DirtyHash:    "def456",
		TFBinVersion: "1.9.0",
		Apply:        &ApplyRecord{Started: now, ExitCode: &ec, WindowID: "@3"},
	}
	if err := s.SaveRun(rec); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadRun(rec.ModulePath, rec.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanExitCode != 2 || got.Summary.Add != 2 || got.Apply == nil || *got.Apply.ExitCode != 0 {
		t.Errorf("round trip mismatch: %+v", got)
	}
}

func TestLoadRunMissing(t *testing.T) {
	s := New(t.TempDir())
	rec, err := s.LoadRun("/nope", "prod")
	if err != nil || rec != nil {
		t.Errorf("want nil,nil got %v, %v", rec, err)
	}
}

func TestPermissions(t *testing.T) {
	s := New(t.TempDir())
	rec := &RunRecord{ModulePath: "/m", Workspace: "w"}
	if err := s.SaveRun(rec); err != nil {
		t.Fatal(err)
	}
	dir, err := s.WorkspaceDir("/m", "w")
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Errorf("workspace dir mode = %v", info.Mode().Perm())
	}
	if info, _ := os.Stat(filepath.Join(dir, "run.json")); info.Mode().Perm() != 0o600 {
		t.Errorf("run.json mode = %v", info.Mode().Perm())
	}
}

func TestExpirePlans(t *testing.T) {
	s := New(t.TempDir())
	old, err := s.PlanFilePath("/m1", "prod")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.PlanFilePath("/m2", "prod")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{old, fresh} {
		if err := os.WriteFile(p, []byte("plan"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}
	n, err := s.ExpirePlans(24 * time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("expired %d, err %v", n, err)
	}
	if s.HasPlanFile("/m1", "prod") {
		t.Error("old plan should be gone")
	}
	if !s.HasPlanFile("/m2", "prod") {
		t.Error("fresh plan should remain")
	}
}

func TestGC(t *testing.T) {
	s := New(t.TempDir())
	alive := t.TempDir() // exists on disk
	dead := filepath.Join(t.TempDir(), "gone")
	for _, p := range []string{alive, dead} {
		if _, err := s.ModuleDir(p); err != nil {
			t.Fatal(err)
		}
	}
	pruned, err := s.GC()
	if err != nil || pruned != 1 {
		t.Fatalf("pruned %d, err %v", pruned, err)
	}
	if _, err := s.LoadRun(alive, "w"); err != nil {
		t.Errorf("alive module state damaged: %v", err)
	}
	if dirs, _ := filepath.Glob(filepath.Join(s.Dir, "modules", "*")); len(dirs) != 1 {
		t.Errorf("module dirs after GC = %d, want 1", len(dirs))
	}
}

func TestIgnoreRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	ig, err := s.LoadIgnore()
	if err != nil || len(ig) != 0 {
		t.Fatalf("empty load: %v %v", ig, err)
	}
	ig["/repo1"] = true
	ig["/repo2/mod//prod"] = true
	if err := s.SaveIgnore(ig); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadIgnore()
	if err != nil {
		t.Fatal(err)
	}
	if !got["/repo1"] || !got["/repo2/mod//prod"] || len(got) != 2 {
		t.Errorf("ignore = %v", got)
	}
}
