# antic (host CLI), antic-agent (guest agent and session runner) and a static
# cloud-hypervisor, all built from this repository's pinned nixpkgs.
{ pkgs }:

let
  inherit (pkgs) lib;
  hostSrc = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../flake.lock
      ../embed.go
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
      ./module.nix
      ./packages.nix
      ./proto.json
    ];
  };
  agentSrc = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd/antic-agent
      ../internal/guest
      ../internal/proto
      ./proto.json
    ];
  };
  goModule = args: pkgs.buildGoModule ({
    version = "0.4.0";
    vendorHash = "sha256-NsfRxs8rzbkXYlacpudkT52zBldyzEbZYIKPOeMxmPA=";
    env.CGO_ENABLED = 0;
    ldflags = [ "-s" "-w" ];
  } // args);
in
{
  antic = goModule {
    pname = "antic";
    src = hostSrc;
    subPackages = [ "cmd/antic" ];
  };

  antic-agent = goModule {
    pname = "antic-agent";
    src = agentSrc;
    subPackages = [ "cmd/antic-agent" ];
  };

  cloud-hypervisor = pkgs.pkgsStatic.cloud-hypervisor;
}
