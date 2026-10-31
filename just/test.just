set shell := ["sh", "-eu", "-c"]
set working-directory := ".."

dev := if env("IN_NIX_SHELL", "") == "" { "nix develop -c" } else { "" }

# Unit tests in the dev shell (go test flags, e.g. "just test unit -run Draft")
unit *flags:
    {{ dev }} go test {{ flags }} ./...

# The packages with property-based tests (*_pbt_test.go), with more cases
pbt cases="20000":
    {{ dev }} go test -count=1 ./internal/tea/operation ./internal/spec/refine ./internal/host/guest ./internal/host/config -rapid.checks={{ cases }}
