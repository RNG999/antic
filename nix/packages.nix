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
      ../internal/domain
      ../internal/guest
      ../internal/proto
      ../internal/tea
      ../internal/types
      ./proto.json
    ];
  };
  goModule = args: pkgs.buildGoModule ({
    version = "0.4.0";
    # The host's and the agent's sources import different modules (the
    # host's tests use pgregory.net/rapid), so each has its own vendorHash.
    vendorHash = "sha256-W61RMiU093FlFvABovS0+dFoMMUVolWHU1+rUvBeigw=";
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
    vendorHash = "sha256-K8+KuNbnkdEAsxWOZEmSGVmtiS6fdqVb4oxshhEM8RU=";
    subPackages = [ "cmd/antic-agent" ];
  };

  cloud-hypervisor = pkgs.pkgsStatic.cloud-hypervisor;

  libexec = pkgs.symlinkJoin {
    name = "antic-libexec";
    paths = [
      pkgs.pkgsStatic.cloud-hypervisor
      pkgs.virtiofsd
      pkgs.passt
      pkgs.nftables
    ];
    postBuild = ''
      for f in "$out/bin"/*; do
        cp --remove-destination "$(realpath "$f")" "$f"
      done
    '';
  };
}
