#!/usr/bin/env bash
# Fresh-install rehearsal (release gate for issue #120): bring up the default Compose stack from a clean state,
# complete first-run setup through the API the way the /setup page does, log in, seed the demo workbook and
# read it back, and check the UI and /healthz. Prints PASS/FAIL per step; exits non-zero on any failure.
#
#   scripts/release/rehearse-install.sh
#
# Environment (all optional):
#   REHEARSAL_NO_BUILD=1       reuse the images from an earlier rehearsal instead of building
#   REHEARSAL_KEEP=1           leave the stack running after the run (prints the cleanup command)
#   REHEARSAL_PRUNE_IMAGES=1   also remove the rehearsal images at the end
#   REHEARSAL_HEALTH_TIMEOUT   seconds to wait for health (default 240)
#   REHEARSAL_TMPDIR           where the work dir (generated env file) lives (default ~/.cache)
#
# The stack runs as project opengtm-rehearsal-<pid> on random free loopback ports and its own subnet. It is
# always removed on exit (`down -v`, scoped to that project). See docs/operations/release-rehearsals.md.
set -uo pipefail
. "$(dirname "${BASH_SOURCE[0]}")/rehearsal-lib.sh"

rh_init "Fresh-install rehearsal"
START=$SECONDS

rh_step "build or reuse the stack images"                 rh_build
rh_step "start the default stack from clean volumes"      rh_up
rh_step "stack becomes healthy (migrate ok, /healthz)"    rh_wait_healthy
rh_step "first-run setup through the API (setup token)"   rh_first_run_setup
rh_step "log in and read /auth/me"                        rh_login
rh_step "seed the demo workbook (compose demo profile)"   rh_seed_demo
rh_step "read the seeded workbook back through the API"   rh_check_demo_workbook
rh_step "UI index and bundle are served"                  rh_check_ui
rh_step "/healthz"                                        rh_check_healthz
rh_info "elapsed $((SECONDS - START))s"
rh_finish
