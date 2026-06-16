# tfmux

A terminal UI for keeping dozens of Terraform repos, root modules and
workspaces under control when you must run `terraform plan` / `apply` from
the command line (no HCP Terraform, no CI/CD applies).

```
tfmux
├ infra-network        main *  ↑2          ← git branch, dirty, ahead/behind
│ ├ envs/prod
│ │   · default        ✓ clean 12m
│ │   · prod           ● +2 ~1 -0 3m      ← outstanding changes stand out
│ └ envs/staging
│     · staging        ⠋ planning
└ k8s-base             feature/foo *
  └ cluster
      · prod           ✗ plan error 1h
```

## What it does

- **Auto-discovers** your IaC estate: configured root dirs → git repos →
  Terraform root modules (any dir whose `.tf` files declare a
  `terraform { backend … }` / `cloud` block) → workspaces
  (`terraform workspace list`). Anything can be hidden with the ignore
  toggle (`i`) and re-enabled later (`Z` shows ignored items).
- **Git status** per repo: branch, uncommitted changes, ahead/behind.
- **Workspace status** at a glance: time since last plan, outstanding
  changes (`● +2 ~1 -0`), plan errors, `STALE` badge when the module's git
  content changed after the plan was taken, apply progress/result.
- **Bulk plans**: `p` plans the selection (workspace, module, repo or marked
  set), `P` plans everything visible. Plans run headless with bounded
  parallelism (`terraform plan -detailed-exitcode -out=…`); same-module jobs
  are serialized, workspaces are selected via `TF_WORKSPACE` so your shell's
  selected workspace is never touched.
- **Applies run in tmux**: `A` applies the saved plan file in a new window
  of a dedicated tmux session — interactive (answers prompts), attachable
  (`t`), and it survives tfmux exiting. Success closes the window; failures
  keep it open for inspection.

## Install

```sh
go install github.com/japsu/tfmux/cmd/tfmux@latest
brew install tmux   # only needed for applies; everything else works without
```

Create `~/.config/tfmux/config.toml` (see [config.example.toml](config.example.toml)):

```toml
roots = ["~/work/iac"]
```

Run `tfmux` for the TUI, or `tfmux ls [--json]` for a scriptable dump.

## Keys

| Key | Action |
|---|---|
| `↑/↓` `j/k` | move |
| `PgUp/PgDn` `C-u/C-d` | jump to top/bottom of screen, then page |
| `←/→` `h/l` | collapse / expand |
| `space` | mark workspace for bulk plan |
| `p` / `P` | plan marked-or-cursor / plan all visible |
| `x` | cancel running job (terraform gets SIGINT, releases its state lock) |
| `enter` | view captured plan output |
| `A` | apply saved plan in tmux |
| `t` | attach to the tmux session (detach with `C-b d` to return) |
| `d` | discard saved plan file |
| `i` / `Z` | toggle ignore / show ignored |
| `I` | `terraform init -upgrade` for the module |
| `r` / `R` | refresh statuses / re-discover repos |
| `w` | re-enumerate workspaces under the cursor (refreshes the cache) |
| `/` | filter |
| `?` | help, `q` quit |

## How it works (and why)

- **Everything is a task on one scheduler.** Workspace enumeration, init,
  plan, and apply are all tasks with the same lifecycle (queued → running →
  done) sharing a single worker pool capped at `parallelism`. The status cell
  distinguishes a spinning *running* task from a dim *queued* one waiting for a
  slot, and the header tallies both.
- **Prioritized queue.** When more work is ready than there are slots, the
  scheduler prefers applies over plans, and plans over enumerations — so a
  plan-all doesn't make you wait on background workspace discovery, and an
  apply you asked for jumps the queue.
- **Plans are headless, applies are tmux.** Plans are read-only; applies are
  interactive, long-running and must not die with the UI — so each apply runs
  in a tmux window that outlives tfmux (a wrapper writes the exit code to a
  file atomically). The apply still holds a pool slot and is watched to
  completion; an apply left running when tfmux exits is re-adopted on restart.
  Canceling (`x`) a *queued* apply drops it before launch; a *running* apply is
  left attached in tmux.
- **`TF_WORKSPACE`, never `workspace select`** — selecting would mutate
  `.terraform/environment` shared with your shell and other jobs.
- **Workspace lists are cached.** Enumerating workspaces hits the backend
  (S3/DynamoDB, etc.) and is slow and easily rate-limited, so the list is
  persisted per module and reused on the next launch. Re-enumerate explicitly
  with `w` (cursor) or `R` (everything) when workspaces change.
- **Per-module serialization.** Any command can lazily turn into a
  `terraform init` (mutates `.terraform/`), so two tasks never run in the
  same module dir concurrently; cross-module parallelism provides the speed.
- **Init is lazy and never `-upgrade`** (that rewrites the lock file —
  explicit `I` only). All commands run `-input=false` so missing credentials
  fail fast instead of hanging a worker.
- **Plan files contain secrets.** They live under `~/.local/state/tfmux`
  with 0700/0600 permissions and are deleted after a successful apply, on
  discard, and after `plan_ttl` (default 24h).
- **Version guard.** Plan files aren't portable across terraform versions
  (or terraform↔tofu); tfmux records the version at plan time and refuses
  to apply with a different binary.

## Development

```sh
go test ./...
```

Tests use a fake `terraform` shell stub (`internal/tftest`) — no cloud
credentials needed, including the end-to-end TUI test (teatest).
