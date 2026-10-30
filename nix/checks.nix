# "nix flake check": Go tests (including the host/guest contract) and builds.
{ pkgs }:

let
  p = import ./packages.nix { inherit pkgs; };
in
{
  go-tests = p.antic.overrideAttrs (old: {
    pname = "antic-tests";
    doCheck = true;
    checkPhase = ''
      runHook preCheck
      go test ./internal/arch ./internal/spec ./internal/domain ./internal/tea/... ./internal/proto ./internal/host/config ./internal/host/instance ./internal/host/share ./internal/host/cli ./internal/host/guest ./internal/host/egress ./internal/host/nixbuild
      runHook postCheck
    '';
  });
  inherit (p) antic-agent;
}
