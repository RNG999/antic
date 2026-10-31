set shell := ["sh", "-eu", "-c"]
set working-directory := ".."

cache := env("XDG_CACHE_HOME", env("HOME") / ".cache") / "antic-e2e"
traces := cache / "traces"
indev := env("IN_NIX_SHELL", "")
dev := if indev == "" { "nix develop -c" } else { "" }
host := if indev == "" { "true" } else { "echo 'run this from a plain shell, not inside nix develop' >&2; exit 1" }

# Build the static antic and the E2E test binary
build:
    {{ host }}
    mkdir -p "{{ cache }}/bin"
    nix develop -c sh -c 'export CGO_ENABLED=0 && go build -o "{{ cache }}/bin/antic" ./cmd/antic && go test -c -tags e2e -o "{{ cache }}/e2e.test" ./e2e'

# Run the E2E tests with saga traces, then validate them (filter: -test.run; env: ANTIC_E2E_*=...)
run filter="." *env: build && traces
    rm -rf "{{ traces }}" && mkdir -p "{{ traces }}"
    cd e2e && env ANTIC_E2E_TRACES="{{ traces }}" ANTIC_E2E_BIN="{{ cache }}/bin/antic" {{ env }} "{{ cache }}/e2e.test" -test.v -test.timeout 6h -test.run '{{ filter }}' > "{{ cache }}/e2e.log" 2>&1 || true
    grep -E '^(--- |    --- |ok|FAIL|PASS)' "{{ cache }}/e2e.log" || true
    if grep -q '^FAIL' "{{ cache }}/e2e.log"; then echo "E2E failed; see {{ cache }}/e2e.log" >&2; exit 1; fi

# Validate the last run's saga traces against formal/saga/Saga.tla
traces:
    ANTIC_SAGA_TRACES="{{ traces }}" {{ dev }} go test -count=1 ./internal/spec/refine -run TestShellTracesRefineSagaModel -v | grep -E 'validated|^(--- |ok|FAIL)'

# Generated command sequences (failures are shrunk and pinned in e2e/testdata/regress)
random count="20" length="8" seed=`date +%Y%m%d`: (run "TestE2ERandomSequences" "ANTIC_E2E_RANDOM=" + count "ANTIC_E2E_LENGTH=" + length "ANTIC_E2E_SEED=" + seed)
