<!--
SPDX-FileCopyrightText: 2026 City of Espoo

SPDX-License-Identifier: LGPL-2.1-or-later
-->

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
  changes (`● +2 ~1 -0`), plan errors, apply progress/result. A `STALE`
  badge on a saved plan, or `needs plan` on a clean one, means the module's
  content (committed or not) changed after the plan was taken; the title bar
  counts how many workspaces need a plan. A module row shows `↑ waf v16→v18`
  when a git module it pins has newer content in that shared repo's local
  checkout, and the title bar counts modules behind.
- **Bulk plans**: `p` plans the selection (workspace, module, repo or marked
  set), `P` plans everything visible. Plans run headless with bounded
  parallelism (`terraform plan -detailed-exitcode -out=…`); same-module jobs
  are serialized, workspaces are selected via `TF_WORKSPACE` so your shell's
  selected workspace is never touched.
- **Applies are tasks too**: `A` applies the saved plan file on the same
  scheduler, with the same live log (`enter`) as a plan. The log is kept
  after the apply, successful or not. Quitting tfmux interrupts whatever is
  running and waits for terraform to stop, so no state lock is left held;
  run tfmux inside tmux or screen if you need an apply to outlive your
  terminal.

## Install

```sh
go install github.com/espoon-voltti/tfmux/cmd/tfmux@latest
```

Run `tfmux init` in the directory your infra repos are checked out under (or
give it another one when asked). It writes a minimal
`~/.config/tfmux/config.toml`:

```toml
roots = ['~/work/iac']
```

It won't overwrite an existing config. For all settings, see
[config.example.toml](config.example.toml).

Run `tfmux` for the TUI, or `tfmux ls [--json]` for a scriptable dump.

`tfmux pending [--json] [--needs-plan]` lists, without launching the TUI or
running terraform, every workspace that needs attention: never planned,
changed since its last plan, a plan waiting to be applied, a plan or apply
error. `--needs-plan` keeps only the rows whose next step is a plan. It reads
the same run records and workspace caches as the TUI, so a module whose
workspaces the TUI has never listed is reported on stderr instead.

`tfmux mcp` serves all of the above (and optionally apply) to an agent over
MCP; see [MCP server](#mcp-server).

`tfmux deps [--reverse] [--json] [FILTER]` prints what each root module
depends on, or with `--reverse` who depends on each shared thing — so
`tfmux deps --reverse evaka-infra-shared//modules/waf` answers "which roots
use waf, at which ref, and how far behind the checkout". `pending` lists the
same behind pins as `module-behind` rows. See [Dependencies](#dependencies).

## Keys

| Key | Action |
|---|---|
| `↑/↓` `j/k` | move |
| `PgUp/PgDn` `C-u/C-d` | jump to top/bottom of screen, then page |
| `←/→` `h/l` | collapse / expand |
| `space` | mark workspace for bulk plan |
| `p` / `P` | plan marked-or-cursor / plan all visible |
| `x` | cancel/interrupt task (queued → dropped; running → SIGINT, terraform stops gracefully; a running apply only from the task pane, after confirming) |
| `T` | task pane: in-flight tasks, then tasks finished this session (newest first; `enter` views their logs); `x` cancel/interrupt, `X` cancel all queued |
| `enter` | see what's happening: view the workspace's latest log — the apply's if one is running or was the last thing done, else the plan's — following it live while the task runs |
| `o` | show `terraform output` for the workspace |
| `A` | apply saved plan — on a module/repo row, confirm then mass-apply every contained workspace whose plan has outstanding changes |
| `d` | discard saved plan file |
| `i` / `Z` | toggle ignore / show ignored |
| `!` | pending only: show just the workspaces that need attention (`needs plan`, a saved plan with changes, a plan or apply error — what `tfmux pending` lists) and the modules with outdated pins, expanded. A snapshot: acting on an item doesn't drop it from the list; toggle off and on to refresh |
| `I` | `terraform init -upgrade` for the module — or every module, on a repo row; when a pinned git module has a newer tag, asks first whether to bump the pin(s) in the `.tf` files |
| `r` / `R` | refresh statuses (git, fingerprints, pins, as at startup) / re-discover repos |
| `w` | re-enumerate workspaces under the cursor (refreshes the cache) |
| `/` | filter — repo/module-row actions (`p`, `A`, `x`, `I`, `w`) then only target what the filter (and `!`) leaves visible |
| `?` | help, `q` quit (asks first while tasks run: they are interrupted and waited for) |

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
- **Every task is a child of tfmux and ends with it.** Applies run exactly
  like plans: headless, `-input=false`, output streamed to a log file. Nothing
  is detached, so nothing can be orphaned: quitting (or a hangup of the
  terminal) interrupts every running terraform with SIGINT, which makes it
  finish in-flight operations and release its state lock, and tfmux waits for
  that before exiting. An apply that was interrupted is recorded as such and
  the workspace needs a new plan — terraform refuses a plan file once state has
  moved on. Persistence across your terminal going away is the job of tmux or
  screen around tfmux, not of tfmux. Canceling (`x`) a *queued* apply drops it
  before launch; a *running* apply is interrupted only from the task pane,
  after confirming.
- **One key to "see what's happening" (`enter`).** Every task (plan, apply,
  output; and on a module row a running init/enumerate) streams its output to
  a log file as it runs, so `enter` tails it in real time (auto-scrolling
  unless you scroll up); a finished task shows the captured log. On a
  workspace it picks the latest activity: a running apply, else a running
  plan, else the last apply, else the last plan. Useful when an enumeration
  stalls on a throttling backend and you want to see what it's doing.
- **`TF_WORKSPACE`, never `workspace select`** — selecting would mutate
  `.terraform/environment` shared with your shell and other jobs. Modules with
  manifest command templates (see below) are the one exception: their
  "workspace" is a logical name handed to the templates as `TFMUX_WORKSPACE`,
  and `TF_WORKSPACE` is never set for them.
- **Workspace lists are cached.** Enumerating workspaces hits the backend
  (S3/DynamoDB, etc.) and is slow and easily rate-limited, so the list is
  persisted per module and reused on the next launch. Re-enumerate explicitly
  with `w` (cursor) or `R` (everything) when workspaces change.
- **Per-module serialization.** Any command can lazily turn into a
  `terraform init` (mutates `.terraform/`), so two tasks never run in the
  same module dir concurrently; cross-module parallelism provides the speed.
- **Init is lazy and never `-upgrade`** (that rewrites the lock file —
  explicit `I` only). All commands run `-input=false` so missing credentials
  fail fast instead of hanging a worker. Tasks queued in a module wait for
  an init queued there; if that init fails, its error shows on the module row
  (`enter` for the full output) and the waiting tasks are canceled rather
  than run against a half-initialized module.
- **Concurrent inits are capped when you use a plugin cache.** A shared
  Terraform/OpenTofu provider plugin cache (`plugin_cache_dir` /
  `TF_PLUGIN_CACHE_DIR`) only tolerates one `terraform init` at a time —
  concurrent inits against it can corrupt the cache. tfmux detects this
  automatically and caps concurrent inits (lazy or explicit) at 1, without
  touching `parallelism` for anything else; override with `init_parallelism`
  if detection ever needs a hand.
- **Staleness is per module, not per repo.** A plan records a fingerprint of
  the module directory: its tree at HEAD plus any uncommitted changes under
  it. Commits elsewhere in the repo leave it alone, so a `git pull` only flags
  the modules it touched. Fingerprints are refreshed on discovery, after each
  plan and on `r` — press `r` after pulling. Plans recorded by tfmux versions
  without fingerprints show as needing a plan once.
- **Pinned git modules are compared against the shared repo's checkout.**
  When a root pins `…//modules/waf?ref=v16` and the repo it names is checked
  out under the roots (matched by its `origin` URL), tfmux compares the
  module's tree at `v16` with the newest tag, with `HEAD` and with the working
  tree, and badges the root `↑ waf v16→v18`, `unreleased` or `uncommitted`.
  tfmux never fetches: `git fetch --tags` the shared repo first. A ref that
  isn't present locally shows as `ref not found locally`; a ref that already
  contains the newest tag (a branch, or a later commit) is never reported
  behind it. Checked on discovery and on `r`, which rescans the pins first
  so a `ref` edited by hand is compared at its new value. `I` on such a root
  offers to rewrite the `source = "…"` attributes pinning an older tag to the
  newest one (in the root's own `.tf` files and those of its in-repo local
  modules) before the `init -upgrade` that would adopt them; the edit is
  uncommitted, so review it with `git diff`.
- **Applying an upstream root marks its downstream roots.** A plan records
  whether it changes outputs. When such a plan is applied, every planned
  workspace of a root that reads that state via `terraform_remote_state` (in
  the workspace the expression maps to, or every workspace when it can't be
  resolved) is marked `needs plan`, and its saved plan file, which embeds the
  old remote-state values, is refused for apply until re-planned. One hop
  only, so roots reading each other's state can't loop. Only applies this
  tfmux watched count: a colleague's apply is invisible until you plan.
- **Plan files contain secrets.** They live under `~/.local/state/tfmux`
  with 0700/0600 permissions and are deleted after a successful apply, on
  discard, and after `plan_ttl` (default 24h).
- **Version guard.** Plan files aren't portable across terraform versions
  (or terraform↔tofu); tfmux records the version at plan time and refuses
  to apply with a different binary.

## The repo manifest

A repo can list its root modules and their valid workspaces explicitly in
**`.terraform-workspaces.json`** at the repo root ("the repo manifest" — not
to be confused with tfmux's own per-module workspace-enumeration cache, which
happens to share the filename `workspaces.json` under the XDG state dir):

```json
[
  { "root_module": "terraform/base", "workspaces": ["staging", "prod"] },
  { "root_module": "terraform/shared", "workspaces": ["default"] }
]
```

- `root_module` is repo-root-relative and slash-separated (`terraform/base`,
  not an absolute path or `./terraform/base`). `_comment` and any other extra
  fields are ignored. Array order only matters to external sweep tooling
  (`bin/tf-all.sh` and friends) that reads the same file — tfmux always sorts
  modules by path for display.
- **The manifest is authoritative for modules it lists**: tfmux never
  enumerates their workspaces from the backend, on discovery or on a forced
  rediscover (`R`). `w` (on a manifest-listed module) and `R` both re-read the
  file rather than touching the backend.
- **A module absent from the manifest is hidden**, exactly like an explicitly
  ignored one, badged "not in manifest" (vs. "ignored") under `Z`. Per-
  workspace ignoring (`i` on a workspace row) still works normally within a
  listed module.
- A listed `root_module` whose directory doesn't exist is reported on its
  repo's row (`manifest: N missing`); a malformed manifest is reported as a
  parse error on the repo's row, and that repo's modules behave as if it had
  no manifest at all.
- A repo with no `.terraform-workspaces.json` is unaffected — workspaces come
  from the enumeration cache / backend as before.

### Command templates

Not every repo selects an environment with a terraform workspace. When each
environment is instead a separate backend picked with `-backend-config`, a
`-var-file`, an `AWS_PROFILE` or the like, an entry can give `init`, `plan`,
`apply` and/or `output` as shell snippets:

```json
[
  {
    "root_module": "terraform/app",
    "workspaces": ["staging", "prod"],
    "init": "terraform init -reconfigure -backend-config=backend-$TFMUX_WORKSPACE.hcl",
    "plan": "terraform plan -var-file=$TFMUX_WORKSPACE.tfvars"
  }
]
```

- tfmux runs a template with `/bin/sh -c` in the module directory, with
  `TFMUX_WORKSPACE` set to the entry's workspace name and `TFMUX_TF_BIN` to the
  resolved terraform binary. Shell parameter expansion works, so
  `${TFMUX_WORKSPACE##*-}` turns `myproject-prod` into `prod`.
- tfmux **appends its own flags** after the snippet: `init` gets `-input=false
  -no-color` (plus `-upgrade` for `I`), `plan` gets `-input=false -no-color
  -detailed-exitcode -out=<planfile>`, `apply` gets `-input=false -no-color
  <planfile>`, `output` gets `-no-color`. Each template must therefore end
  with the terraform subcommand it names. A verb with no template runs
  `"$TFMUX_TF_BIN" <verb>` plus those flags. Don't put `-var` or `-var-file`
  in `apply`: it applies the saved plan file, which already carries them.
- For such a module the workspace is a **logical name**: `TFMUX_WORKSPACE`
  does not carry over to `TF_WORKSPACE`, which tfmux never sets for any of
  the four verbs. If the environment is also a real terraform workspace, say
  so in every template that reads state, `output` included:
  `"output": "TF_WORKSPACE=$TFMUX_WORKSPACE terraform output"`.
- If `init` references `$TFMUX_WORKSPACE`, the module directory can be
  initialised for only one workspace at a time. tfmux remembers which one
  (`init.workspace` in the module's state dir) and re-runs `init` before a
  plan, output or apply for a different workspace.

## MCP server

`tfmux mcp` serves tfmux's knowledge and actions to an agent that must not
hold your credentials — typically a coding agent inside a sandbox (the
author's is [nono](https://github.com/always-further/nono)) with no AWS
access and no view of tfmux's config or state. tfmux runs outside with
everything; the agent gets tools.

- **Read-only by default**: `list`, `pending`, `deps`, `plan_log`, plus
  `plan`, `output`, `enumerate_workspaces` and `refresh`. The last four run
  terraform on the host, with your credentials, but change nothing; plans
  are saved for you to apply in the TUI. Plan JSON and plan files are never
  exposed, only terraform's text output (sensitive values stay redacted).
- **`--all`** (or `tools = "all"` under `[mcp]` in the config) adds `apply`,
  `init_upgrade` and `discard_plan`. `apply` has the same gate as `A` in the
  TUI, waits for the apply, and returns terraform's output when it fails. A
  client that disconnects mid-apply does not interrupt it; stopping the server
  does, waiting for terraform to release its locks.
- **Transport**: Streamable HTTP on `127.0.0.1:7411` (`listen` in config,
  `--listen` to override) behind a bearer token stored 0600 in the state dir.
  `--no-auth` is accepted on loopback only. `--stdio` serves a client on the
  same machine that spawns the server itself. Every tool call is logged to
  stderr.

Setup for Claude Code in a sandbox:

```sh
tfmux mcp --print-config          # prints the `claude mcp add …` line
claude mcp add -s user --transport http tfmux http://127.0.0.1:7411/mcp --header "Authorization: Bearer …"
tfmux mcp                         # leave running; the sandboxed agent connects over loopback
```

The TUI and `tfmux mcp` can run side by side: both persist to the same state
dir and serialize work per module with a lock.

## Dependencies

tfmux reads every root module's `.tf` files (statically — no `terraform
init` needed, so it reports what is written, not what was last initialised)
and records:

- **Git module calls** (`git::…`, `git@host:…`, `github.com/…` sources),
  split into repo, subdirectory and `ref`, followed through local modules.
- **Local module calls** (`./` and `../` sources), transitively. Those inside
  the repo are part of the module's fingerprint, so editing `../modules/x`
  flags every root calling it as needing a plan. Sources outside the repo are
  listed but not fingerprinted.
- **Remote states** read via `data "terraform_remote_state"`, matched to the
  root module whose backend block has the same bucket and key. The workspace
  expression is classified as the same workspace, a literal, a template over
  `terraform.workspace`, or unresolved (anything else, e.g. `local.*`).
- The root's **own backend**, when its bucket and key are literal (partial
  `-backend-config` setups are not matched).

Registry modules are ignored. Root modules that read each other's state are
fine: only direct edges are stored, nothing walks the graph.

## Development

```sh
go test ./...
```

Tests use a fake `terraform` shell stub (`internal/tftest`) — no cloud
credentials needed, including the end-to-end TUI test (teatest).

## License

tfmux is published under **LGPL-2.1-or-later** license. Please refer to
[LICENSE](LICENSE) for further details.

### Check licensing compliance

This repository targets [REUSE](https://reuse.software/) compliance by utilizing
the [reuse CLI tool](https://git.fsfe.org/reuse/tool). Bulk-licensing for files
that cannot carry a header (e.g. `go.sum`) is applied via
[`REUSE.toml`](./REUSE.toml). Compliance is enforced in CI (see
[`.github/workflows/ci.yml`](./.github/workflows/ci.yml)).

To manually check that the repository is compliant (e.g. before submitting a
pull request), run:

```sh
./bin/add-license-headers.sh --lint-only
```

### Automatically add licensing headers

To **attempt** to automatically add licensing headers to all source files, run:

```sh
./bin/add-license-headers.sh
```
