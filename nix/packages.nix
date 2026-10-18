# hanare (host commands and the in-VM "hanare vm" commands, one static binary) and the
# static vhost-device-vsock backend it runs on the host.
{ pkgs }:

let
  inherit (pkgs) lib;
in
{
  hanare = pkgs.buildGoModule {
    pname = "hanare";
    version = "0.3.0";
    src = lib.fileset.toSource {
      root = ../.;
      fileset = lib.fileset.unions [
        ../go.mod
        ../go.sum
        ../main.go
        ../host
        (lib.fileset.fileFilter (f: !f.hasExt "o") ../guest)
      ];
    };
    vendorHash = "sha256-8HzxNZNWk7Co6wdpx5iJAJIJGdIp7BHQGbAXuzoxZKo=";
    nativeBuildInputs = [ pkgs.llvmPackages.clang-unwrapped ];
    env.CGO_ENABLED = 0;
    ldflags = [ "-s" "-w" ];
    # The BPF objects are embedded into the binary (guest/*.go).
    preBuild = ''
      for f in guest/bpf/*.bpf.c; do
        clang -O2 -g -target bpf -D__TARGET_ARCH_x86 \
          -I${pkgs.libbpf}/include -I${pkgs.linuxHeaders}/include \
          -c "$f" -o "''${f%.c}.o"
      done
    '';
    # The module fetch only needs go.mod/go.sum.
    overrideModAttrs = _: { preBuild = ""; };
  };

  # Its tests need AF_VSOCK, which the build sandbox lacks.
  vhost-device-vsock = pkgs.pkgsStatic.vhost-device-vsock.overrideAttrs { doCheck = false; };
}
