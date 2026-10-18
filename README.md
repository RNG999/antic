# hanare

A NixOS VM for running CLI tools, especially ones an AI agent drives,
isolated from the host. Everything runs as your own user, with no root
daemon. Host directories are shared at the same path, and `hanare run`
executes a command inside the VM in your current directory.

## How it works

- **VM:** QEMU (KVM) runs as a transient user unit, `hanare.service`. The
  unit is sandboxed with `ProtectHome=tmpfs` and `ProtectSystem=strict`, so
  QEMU sees only the shared directories and hanare's own files.
- **Shares:** one rootless `virtiofsd` per shared directory. Only your
  UID/GID is mapped, so files the guest writes are owned by you on the host.
  `ro` is enforced by virtiofsd itself.
- **Network:** `passt` without host-loopback mapping, so the guest cannot
  reach services on the host's `127.0.0.1`. Unless `hanare.allowLan` is set,
  the guest firewall also blocks private, link-local and CGNAT (Tailscale)
  ranges.
- **Access:** SSH over vsock with a dedicated key. The host passes the public
  key as a systemd credential at boot, so the guest flake holds no key. The
  guest has no passwords, no sudo and no root login.
- **Guest filesystem:**
  - `/` is tmpfs and is reset on every boot.
  - `/nix` and the SSH host key are kept on disk under `/persist`.
  - Only the shared directories hold data that lasts.

## Requirements

Arch Linux packages: `qemu-base virtiofsd edk2-ovmf passt`, plus systemd
v256 or later.

You also need Nix with flakes. It can run rootless, for example as a static
`nix` binary with `store = local?root=$HOME/.local/share/nix-root`.

## Layout

```
~/.local/src/hanare/      this repository (CLI + nixosModules.default)
~/.config/hanare/
├── hanare.toml           host settings: VM size, shares, exposed commands
└── guest/                your guest flake (nixosConfigurations.hanare)
~/.local/state/hanare/    VM disk, SSH key, pinned guest host key
```

None of these may be inside a shared directory: a guest that could write
them would control what the host builds and runs. `hanare` refuses to share
`~/.config` and `~/.local`.

## Setup

```sh
git clone https://github.com/RNG999/hanare ~/.local/src/hanare
mkdir -p ~/.config/hanare ~/.local/state/hanare/ssh
cp ~/.local/src/hanare/examples/hanare.toml ~/.config/hanare/
cp -r ~/.local/src/hanare/examples/guest ~/.config/hanare/guest   # set hanare.user
ssh-keygen -t ed25519 -N '' -C hanare -f ~/.local/state/hanare/ssh/id_ed25519
install -m 0755 "$(nix build --no-link --print-out-paths path:$HOME/.local/src/hanare)/bin/hanare" ~/.local/bin/
hanare reinstall && hanare up
```

If you use a rootless Nix store, prefix the build output path with the
store root.

## Usage

```
hanare run CMD...           run CMD in the VM, in the current directory
hanare shell                interactive shell in the VM
hanare up | down | status | logs
hanare switch               apply the guest flake to the running VM (nixos-rebuild switch)
hanare reinstall            replace the VM disk with an image built from the guest flake
hanare update               rebuild ~/.local/src/hanare and reinstall the hanare binary
hanare share [--ro] [--save] DIR...   hot-plug a share; --save also appends it to hanare.toml
hanare unshare DIR...
hanare link                 sync ~/.local/bin links with commands.expose in hanare.toml
```

Add the tools you need to `environment.systemPackages` in your guest
flake's `configuration.nix`, then run `hanare switch`.

`switch` and `reinstall` stop early if the guest flake's lock pins an older
state of a local-path input, such as a local hanare checkout. `switch` also
stops if a local-path input lies outside the guest flake, because the guest
builds from a read-only copy of that directory alone.

## Caveats

- **Shared repositories:** a guest with write access to a git repository can
  plant hooks or `.git/config` settings. The host then runs them the next
  time it uses git in that repository. With `hanare.protectGit` (the
  default), the guest bind-mounts every `.git` inside writable shares
  read-only. This happens at boot, on `hanare share`, and every 30s after
  that. Unprivileged guest processes cannot undo these mounts. A repository
  created after the last pass stays writable until the next one, and git
  operations that write `.git` must run on the host.
- **Linked tool names:** links in `~/.local/bin` lose to host commands that
  come earlier in `PATH`. `hanare link` skips those names.
- **Refused shares:** `hanare` will not share `$HOME` itself, `~/.ssh`,
  `~/.gnupg`, `~/.claude`, `~/.config` or `~/.local`, nor any directory
  that contains them.
