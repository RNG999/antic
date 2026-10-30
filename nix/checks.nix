# "nix flake check": Go tests (including the host/guest contract) and builds.
{ pkgs }:

let
  p = import ./packages.nix { inherit pkgs; };
in
{
  go-tests = p.antic.overrideAttrs (old: {
    pname = "antic-tests";
    doCheck = true;
    # internal/spec/refine proves the egress rules equal the spec with Z3.
    nativeCheckInputs = (old.nativeCheckInputs or [ ]) ++ [ pkgs.z3 ];
    checkPhase = ''
      runHook preCheck
      go test ./internal/arch ./internal/spec/... ./internal/domain ./internal/tea/... ./internal/proto ./internal/host/config ./internal/host/instance ./internal/host/share ./internal/host/cli ./internal/host/guest ./internal/host/egress ./internal/host/nixbuild
      runHook postCheck
    '';
  });
  inherit (p) antic-agent;
}
