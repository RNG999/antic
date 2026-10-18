# hanare (host CLI), banken (guest agent and BPF programs) and a static
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
      (lib.fileset.fileFilter (f: !f.hasExt "o") ../internal)
      ./proto.json
    ];
  };
  goModule = args: pkgs.buildGoModule ({
    version = "0.4.0";
    inherit src;
    vendorHash = "sha256-VUZzKblYW6bt8QH0bH9+OHbR+2On/daOg+5RnGJg8bE=";
    env.CGO_ENABLED = 0;
    ldflags = [ "-s" "-w" ];
  } // args);
in
{
  hanare = goModule {
    pname = "hanare";
    subPackages = [ "cmd/hanare" ];
  };

  banken = goModule {
    pname = "banken";
    subPackages = [ "cmd/banken" ];
    nativeBuildInputs = [ pkgs.llvmPackages.clang-unwrapped ];
    # The BPF objects are embedded into the binary.
    preBuild = ''
      for f in internal/guest/*/bpf/*.bpf.c; do
        clang -O2 -g -target bpf -D__TARGET_ARCH_${pkgs.stdenv.hostPlatform.linuxArch} \
          -I${pkgs.libbpf}/include -I${pkgs.linuxHeaders}/include \
          -c "$f" -o "''${f%.c}.o"
      done
    '';
    # The module fetch only needs go.mod/go.sum.
    overrideModAttrs = _: { preBuild = ""; };
  };

  cloud-hypervisor = pkgs.pkgsStatic.cloud-hypervisor;
}
