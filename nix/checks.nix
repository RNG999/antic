# "nix flake check": Go tests (including the host/guest contract) and builds.
{ pkgs }:

let
  p = import ./packages.nix { inherit pkgs; };
in
{
  go-tests = p.hanare.overrideAttrs (old: {
    pname = "hanare-tests";
    doCheck = true;
    checkPhase = ''
      runHook preCheck
      go test ./internal/proto ./internal/host/config ./internal/host/instance ./internal/host/share
      runHook postCheck
    '';
  });
  inherit (p) banken;
}
