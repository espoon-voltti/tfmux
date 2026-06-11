// Package tftest provides a fake terraform binary (a shell script) for
// testing tfexec and runner without real cloud credentials.
package tftest

import (
	"os"
	"path/filepath"
	"testing"
)

// Script is a shell script standing in for the terraform binary. Behavior is
// driven by env knobs:
//
//	TFMUX_FAKE_LOG        append "<pid> <pwd> <TF_WORKSPACE> <args>" per call,
//	                      bracketed by "start"/"end" lines with ns timestamps
//	TFMUX_FAKE_PLAN_EXIT  exit code for `plan` (default 0)
//	TFMUX_FAKE_NEED_INIT  if set, plan/workspace-list fail with an
//	                      init-required message until `init` has run
//	                      (tracked via a sentinel file next to the log)
//	TFMUX_FAKE_SLEEP      seconds to sleep before answering (concurrency tests)
//	TFMUX_FAKE_SHOW_JSON  file whose contents `show -json` emits
const Script = `#!/bin/sh
ARGS="$*"
log() { [ -n "$TFMUX_FAKE_LOG" ] && echo "$1 $$ $PWD ${TF_WORKSPACE:-<none>} $ARGS" >> "$TFMUX_FAKE_LOG"; }
log start
[ -n "$TFMUX_FAKE_SLEEP" ] && sleep "$TFMUX_FAKE_SLEEP"
finish() { log end; exit "$1"; }
case "$1" in
  init)
    [ -n "$TFMUX_FAKE_NEED_INIT" ] && touch "$TFMUX_FAKE_LOG.initialized"
    mkdir -p .terraform
    echo "Terraform has been successfully initialized!"
    finish 0
    ;;
  workspace)
    if [ -n "$TFMUX_FAKE_NEED_INIT" ] && [ ! -f "$TFMUX_FAKE_LOG.initialized" ]; then
      echo 'Error: Backend initialization required, please run "terraform init"' >&2
      finish 1
    fi
    printf '  default\n* prod\n  staging\n'
    finish 0
    ;;
  plan)
    if [ -n "$TFMUX_FAKE_NEED_INIT" ] && [ ! -f "$TFMUX_FAKE_LOG.initialized" ]; then
      echo 'Error: Backend initialization required, please run "terraform init"' >&2
      finish 1
    fi
    out=""
    for a in "$@"; do case "$a" in -out=*) out="${a#-out=}";; esac; done
    [ -n "$out" ] && echo "fake plan" > "$out"
    echo "Plan: 1 to add, 0 to change, 0 to destroy."
    finish "${TFMUX_FAKE_PLAN_EXIT:-0}"
    ;;
  show)
    cat "${TFMUX_FAKE_SHOW_JSON:?TFMUX_FAKE_SHOW_JSON not set}"
    finish 0
    ;;
  version)
    echo '{"terraform_version":"1.9.9"}'
    finish 0
    ;;
  apply)
    echo "Apply complete!"
    finish 0
    ;;
esac
echo "unknown subcommand $1" >&2
finish 1
`

// Write installs the fake binary into dir and returns its path.
func Write(t testing.TB, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "terraform")
	if err := os.WriteFile(path, []byte(Script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
