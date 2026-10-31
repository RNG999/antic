# Naming conventions

The rules the code follows, so that the next name fits the ones around it.
`internal/arch` enforces the export and layering rules; the rest is review.

## Files

- **By subject, not by technique or by kind of operation.** A file holds one
  subject (`guest_shares.go`, `journal.go`, `vm.go`), never a grab bag
  (`misc.go`, `ops.go`, `util.go`).
- **CLI commands:** one file per command group, named after the command path:
  `guest.go` (the `guest` command itself), `guest_<topic>.go` for its
  subcommands (`guest_lifecycle.go`, `guest_shares.go`, ...), `<command>.go`
  for top-level commands (`run.go`, `update.go`).
- **Tests** sit next to their subject (`journal_test.go` tests
  `journal.go`). A test by a particular technique says so in its file name:
  `<subject>_<technique>_test.go`, e.g. `saga_pbt_test.go`,
  `egress_smt_test.go`, `saga_tla_test.go`.

## Identifiers

- **No abbreviations** beyond Go's and this code's established ones (`cfg`,
  `ctx`, `tx`, `err`, `ID`, `TCP`, `VM`). Write `Packages`, not `Pkgs`;
  `ProtoString`, not `ProtoStr`.
- **CLI command constructors** are `<command path>Cmd`:
  `guestPsCmd`, `cacheLsCmd`, `runCmd`.
- **Say what it is, not where it is used:** `WorkDir` (the working directory
  inside a share), not `Dir`; `runChange`, not `commit` (a saga has a commit
  phase of its own).
- **One name per concept.** When two places name the same thing, one of them
  changes (`Draft.NewFile` and the journal's `new_file`).
- **A named check instead of a repeated condition:** `config.RealPath`,
  `Port.Overlaps`, `domain.Printable`, `isLowerAlnum`.

## Tests

- `Test<Subject><Property>`: `TestSagaCoreRefinesModel`,
  `TestDraftEditsKeepComments`. The technique is in the file name, not in
  the test name; two tests of one property differ by what they cover
  (`TestEgressRulesRefineReach` over a fixed universe,
  `TestEgressRulesRefineReachOnRandomInputs` over random inputs).
- E2E tests are `TestE2E<Subject>`; scenario names say what they check and
  start with the divergence they cover, if any (`D7 host port used by another
  guest`).

## Environment variables

- `ANTIC_<AREA>_<THING>`, one name per concept: `ANTIC_SAGA_TRACE` (the file
  antic appends its saga events to), `ANTIC_E2E_TRACES` (the directory of
  the E2E traces, for both the harness and the validator).
