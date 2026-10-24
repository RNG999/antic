# antic (host CLI), antic-agent (guest agent and session runner) and a static
# cloud-hypervisor, all built from this repository's pinned nixpkgs.
{ pkgs }:

let
  inherit (pkgs) lib;
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
      ./proto.json
    ];
  };
  goModule = args: pkgs.buildGoModule ({
    version = "0.4.0";
    inherit src;
    vendorHash = "sha256-NsfRxs8rzbkXYlacpudkT52zBldyzEbZYIKPOeMxmPA=";
    env.CGO_ENABLED = 0;
    ldflags = [ "-s" "-w" ];
  } // args);
in
{
  antic = goModule {
    pname = "antic";
    subPackages = [ "cmd/antic" ];
  };

  antic-agent = goModule {
    pname = "antic-agent";
    subPackages = [ "cmd/antic-agent" ];
  };

  cloud-hypervisor = pkgs.pkgsStatic.cloud-hypervisor;
}
