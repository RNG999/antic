# "nix flake check": Go tests (including the host/guest contract) and builds.
{ pkgs }:

let
  p = import ./packages.nix { inherit pkgs; };
  # The package's source plus the TLA+ model the tests check and replay
  # (kept out of the package itself, so the model's edits do not rebuild it).
  testSrc = pkgs.lib.fileset.toSource {
    root = ../.;
    fileset = pkgs.lib.fileset.unions [
      (pkgs.lib.fileset.fromSource p.antic.src)
      ../formal/saga
    ];
  };
in
{
  go-tests = p.antic.overrideAttrs (old: {
    pname = "antic-tests";
    src = testSrc;
    doCheck = true;
    # internal/spec/refine proves the egress rules equal the spec with Z3,
    # model-checks formal/saga with TLC and replays its graph on the core.
    nativeCheckInputs = (old.nativeCheckInputs or [ ]) ++ [ pkgs.z3 pkgs.tlaplus ];
    checkPhase = ''
      runHook preCheck
      go test ./internal/arch ./internal/spec/... ./internal/domain ./internal/tea/... ./internal/proto ./internal/host/config ./internal/host/instance ./internal/host/share ./internal/host/cli ./internal/host/guest ./internal/host/egress ./internal/host/nixbuild
      runHook postCheck
    '';
  });
  inherit (p) antic-agent;
}
