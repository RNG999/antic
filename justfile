# antic development tasks: "just" lists them.
#
# The E2E tasks run antic for real on this host as this user (KVM,
# systemd --user) and only touch guests named e2e-*. They must run outside
# "nix develop" (its namespace hides the user's systemd), so the binaries
# are built static inside it and run outside. A rootless Nix cannot start
# "nix develop" from inside another Nix namespace, so run just itself from
# a plain shell (install it on the host); inside "nix develop" only the
# tasks that need no host run, directly.

set shell := ["sh", "-eu", "-c"]

cache := env("XDG_CACHE_HOME", env("HOME") / ".cache") / "antic-e2e"
traces := cache / "traces"
indev := env("IN_NIX_SHELL", "")
dev := if indev == "" { "nix develop -c" } else { "" }
host := if indev == "" { "true" } else { "echo 'run this from a plain shell, not inside nix develop' >&2; exit 1" }

default:
    @just --list

# Every change: nix flake check (unit tests, PBT, Z3, TLC, Spacer, replay) and lint
quick: check lint

# nix flake check
check:
    {{ host }}
    nix flake check

# Lint the issues introduced since the previous commit
lint:
    {{ dev }} golangci-lint run --new-from-rev=HEAD~1 ./...

# Unit tests in the dev shell (pass go test flags, e.g. "just test -run PBT")
test *flags:
    {{ dev }} go test {{ flags }} ./...

# Property-based tests with more cases
pbt checks="20000":
    {{ dev }} go test -count=1 ./internal/tea/operation ./internal/spec/refine ./internal/host/guest -run PBT -rapid.checks={{ checks }}

# Build the static antic and the E2E test binary
build-e2e:
    {{ host }}
    mkdir -p "{{ cache }}/bin"
    nix develop -c sh -c 'export CGO_ENABLED=0 && go build -o "{{ cache }}/bin/antic" ./cmd/antic && go test -c -tags e2e -o "{{ cache }}/e2e.test" ./e2e'

# E2E on this host with saga traces, then trace validation (run: test filter; env: ANTIC_E2E_*=...)
e2e run="." *env: build-e2e
    rm -rf "{{ traces }}" && mkdir -p "{{ traces }}"
    cd e2e && env ANTIC_E2E_TRACES="{{ traces }}" ANTIC_E2E_BIN="{{ cache }}/bin/antic" {{ env }} "{{ cache }}/e2e.test" -test.v -test.timeout 6h -test.run '{{ run }}' > "{{ cache }}/e2e.log" 2>&1 || true
    grep -E '^(--- |    --- |ok|FAIL|PASS)' "{{ cache }}/e2e.log" || true
    if grep -q '^FAIL' "{{ cache }}/e2e.log"; then echo "E2E failed; see {{ cache }}/e2e.log" >&2; exit 1; fi
    {{ just_executable() }} traces

# Validate the last E2E run's saga traces against formal/saga/Saga.tla
traces:
    ANTIC_SAGA_TRACES="{{ traces }}" {{ dev }} go test -count=1 ./internal/spec/refine -run TestShellTracesRefineSagaModel -v | grep -E 'validated|^(--- |ok|FAIL)'

# Generated E2E command sequences (failures are shrunk and pinned in e2e/testdata/regress)
random n="20" length="8" seed=`date +%Y%m%d`:
    {{ just_executable() }} e2e TestE2ERandomSequences ANTIC_E2E_RANDOM={{ n }} ANTIC_E2E_LENGTH={{ length }} ANTIC_E2E_SEED={{ seed }}

# Every day: larger PBT, then every E2E test with generated sequences and more concurrent rounds
nightly: (pbt "20000")
    {{ just_executable() }} e2e . ANTIC_E2E_RANDOM=20 ANTIC_E2E_SEED=`date +%Y%m%d` ANTIC_E2E_ROUNDS=8
