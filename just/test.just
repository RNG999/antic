set shell := ["sh", "-eu", "-c"]
set working-directory := ".."

dev := if env("IN_NIX_SHELL", "") == "" { "nix develop -c" } else { "" }

# Unit tests in the dev shell (go test flags, e.g. "just test unit -run PBT")
unit *flags:
    {{ dev }} go test {{ flags }} ./...

# Property-based tests with more cases
pbt cases="20000":
    {{ dev }} go test -count=1 ./internal/tea/operation ./internal/spec/refine ./internal/host/guest -run PBT -rapid.checks={{ cases }}
