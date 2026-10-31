# antic development tasks. "just" lists them with their subcommands
# ("just test ...", "just e2e ...").
#
# The E2E tasks run antic for real on this host as this user (KVM, systemd
# --user) and only touch guests named e2e-*. A rootless Nix cannot start
# "nix develop" from inside another Nix namespace and the E2E tests must
# run outside it, so run just from a plain shell (install it on the host);
# inside "nix develop" only the tasks that need no host run, directly.

set shell := ["sh", "-eu", "-c"]

# Unit and property-based tests
mod test 'just/test.just'

# End-to-end tests on this host
mod e2e 'just/e2e.just'

indev := env("IN_NIX_SHELL", "")
dev := if indev == "" { "nix develop -c" } else { "" }
host := if indev == "" { "true" } else { "echo 'run this from a plain shell, not inside nix develop' >&2; exit 1" }

default:
    @just --list --list-submodules

# Every change: nix flake check (unit tests, PBT, Z3, TLC, Spacer, replay) and lint
check: lint
    {{ host }}
    nix flake check

# Lint the issues introduced since the previous commit
lint:
    {{ dev }} golangci-lint run --new-from-rev=HEAD~1 ./...

# Every day: larger PBT, then every E2E test with generated sequences and more concurrent rounds
nightly:
    {{ just_executable() }} test pbt 20000
    {{ just_executable() }} e2e run . ANTIC_E2E_RANDOM=20 ANTIC_E2E_SEED=`date +%Y%m%d` ANTIC_E2E_ROUNDS=8
