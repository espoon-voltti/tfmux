<!--
SPDX-FileCopyrightText: 2026 City of Espoo

SPDX-License-Identifier: LGPL-2.1-or-later
-->

# CLAUDE.md

Guidance for working in this repo. For user-facing docs (what tfmux does, keys,
config), read [README.md](README.md) — this file covers how to build, test and
not break things.

## What this is

`tfmux` is a terminal UI (Bubble Tea) for orchestrating `terraform plan`/`apply`
across many git repos → root modules → workspaces, when applies run from the CLI
(no HCP Terraform, no CI/CD). Module path: `github.com/espoon-voltti/tfmux`,
Go 1.26.

## Commands

Tasks are defined in `Taskfile.yml` (run with [`task`](https://taskfile.dev)),
but plain `go` works too:

- `task check` — **the pre-commit gate**: `fmt:check` → `vet` → `test`. Run this
  before committing.
- `task test` / `go test ./...` — full suite. `task test -- -run TestFoo` to
  filter. `task test:race` for the race detector.
- `task build` → `dist/tfmux`. `task run -- ls` to run a subcommand.
- `task fmt` — `gofmt -w .`. Sources must be gofmt-clean (CI enforces).

Tests need **no cloud credentials**: a fake `terraform` shell stub in
`internal/tftest` stands in for the real binary, including the end-to-end TUI
test (`internal/ui/e2e_test.go`, via teatest).

## Layout

- `cmd/tfmux/main.go` — entrypoint and subcommands: `tui` (default), `ls
  [--json]`, `import-workspaces`, `version`.
- `internal/domain` — shared model (Repo → Module → Workspace) and statuses. Has
  **no dependencies on other tfmux packages**; keep it that way so every layer
  can import it.
- `internal/ui` — the Bubble Tea TUI (the bulk of the code). `model.go` is the
  root model; `tree.go`, `tasks.go`, `styles.go`, `keys.go`, `msgs.go`.
- `internal/discovery` — walks roots → repos → root modules.
- `internal/runner` — the task scheduler (enumerate/init/plan/apply as tasks on
  one bounded worker pool, prioritized queue).
- `internal/tfexec` — builds/runs terraform (or OpenTofu) commands.
- `internal/tmuxctl` — drives tmux for applies.
- `internal/state` — persists machine-owned state under XDG state dir.
- `internal/config` / `internal/paths` — TOML config and XDG path resolution.
- `internal/gitstatus` — branch/dirty/ahead-behind snapshot.

## UI conventions (internal/ui)

- **`Update` is I/O-free.** Every side effect lives in a `tea.Cmd` (see
  `msgs.go`), so message → state transitions are directly unit-testable. Don't
  do file/network/exec work inside `Update`; emit a command.
- **Title bar convention:** the first row keeps `tfmux` visible and shows
  context for the current screen via `headerContext()` — data-scale counts on
  the main view, the open plan's `repo · module · workspace` in the detail view,
  task count on the task pane. Counts respect ignores (count all only when `Z`
  reveals ignored items), mirroring `flatten`'s logic.
- `focusArea` (tree / detail / filter / tasks) is the modal state driving both
  key handling and rendering.
- Tests live next to the code (`*_test.go`) with helpers in `model_test.go`
  (`fixtureModel`, `enumerated`, `keyPress`). Add a test for new behavior.

## Invariants — don't regress these

These are deliberate (see README "How it works"); changing them has real
consequences:

- Plans use **`TF_WORKSPACE`, never `terraform workspace select`** — selecting
  mutates `.terraform/environment` shared with the user's shell.
- **Per-module serialization:** two tasks never run in the same module dir at
  once (any command can lazily `init` and mutate `.terraform/`).
- **`init` is lazy and never `-upgrade`** implicitly (that rewrites the lock
  file). All commands run `-input=false`.
- **Plan files contain secrets:** stored under the XDG state dir with 0700/0600
  perms, deleted after a successful apply, on discard, and after `plan_ttl`.
- **Version guard:** plan files aren't portable across terraform/tofu versions;
  apply refuses a different binary than the one used at plan time.

## Licensing (REUSE)

Every source file carries an SPDX header (`City of Espoo`,
`LGPL-2.1-or-later`); files that can't (e.g. `go.sum`) are covered by
`REUSE.toml`. CI enforces compliance.

- Check: `./bin/add-license-headers.sh --lint-only`
- Add headers to new files: `./bin/add-license-headers.sh`

## Workflow notes

This project is pre-alpha — commit directly to `main` (no feature branches).
