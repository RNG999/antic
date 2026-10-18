# hanare

A NixOS VM for running CLI tools, especially ones an AI agent drives,
isolated from the host. Everything runs as your own user, with no root
daemon. Host directories are shared at the same path, and `hanare run`
executes a command inside the VM in your current directory.

## How it works

- **Units:** everything runs as transient user units, visible with
  `systemctl --user list-units 'hanare*'`:
  - `hanare.service` runs QEMU;
  - `hanare-net.service` runs pasta and passt;
  - `hanare-vsock.service` runs vhost-device-vsock;
  - `hanare-share-<tag>.service` runs one virtiofsd per share.
- **VM:** QEMU (KVM) runs in `hanare.service`.
  - The unit is sandboxed with `ProtectHome=tmpfs`, `ProtectSystem=strict`
    and `RestrictAddressFamilies=AF_UNIX`. QEMU sees only the shared
    directories and hanare's own files, not the SSH client key.
  - QEMU runs with `-nodefaults`, nested virtualization off and
    `-sandbox on`.
- **Shares:** one rootless `virtiofsd` per shared directory. Only your
  UID/GID is mapped, so files the guest writes are owned by you on the host.
  `ro` is enforced by virtiofsd itself.
- **Network:** `passt` runs in a rootless network namespace provided by
  `pasta` (`hanare-net.service`).
  - Neither maps the host's loopback, so the guest cannot reach services on
    the host's `127.0.0.1`.
  - Unless `network.allow_lan` is set, nftables in that namespace rejects
    private, link-local (including cloud metadata) and CGNAT (Tailscale)
    destinations. The rules sit outside the guest, so guest root cannot
    remove them.
- **Access:** SSH over vsock with a dedicated key.
  - vsock goes through `vhost-device-vsock`, so the host end is an AF_UNIX
    socket in hanare's 0700 runtime directory. Other host users cannot reach
    it, and the host kernel does not parse guest vsock traffic.
  - The host generates the guest's SSH host key and pins it.
  - At boot, the host passes the guest's host key and its own public key
    (marked `restrict,pty`) as systemd credentials, so the guest flake holds
    no key.
  - sshd allows only that user and no forwarding. The guest has no
    passwords, no sudo and no root login.
  - Each `hanare run` is one sshd unit instance in the guest (PAM is off,
    so the session stays in that unit's cgroup).
  - `hanare run --offline`, and the commands in `commands.offline`, use a
    second sshd unit with no network at all: a private network namespace
    plus `IPAddressDeny=any`. The resolver, nsncd, D-Bus and the nix-daemon
    are hidden there.
- **Guest kernel (BPF and sysctl):**
  - Unless `hanare.restrictUserNamespaces = false`, a BPF LSM program
    denies user namespaces to everyone but root, removing a common first
    step of kernel exploits.
  - io_uring and unprivileged BPF are disabled, and perf, kernel pointers
    and dmesg are restricted.
  - With `hanare.audit` (default on), a BPF program logs the guest user's
    program executions, outgoing connections and files opened for writing,
    per session; see `hanare audit`.
- **Guest filesystem:**
  - `/` is tmpfs and is reset on every boot.
  - `/nix` is kept on disk under `/persist`. Shortly after every boot it is
    checked against the Nix database, and `hanare status` shows the result.
  - Only the shared directories hold data that lasts.

## Requirements

Arch Linux packages: `qemu-base virtiofsd edk2-ovmf passt nftables`, plus
systemd v256 or later.

You also need Nix with flakes. It can run rootless, for example as a static
`nix` binary with `store = local?root=$HOME/.local/share/nix-root`.

## Repository

```
main.go           entry point of the single hanare binary
host/             commands that run on the host (VM management, run, share, ...)
guest/            what runs in the VM: module.nix (nixosModules.default), the
                  "hanare guest ..." commands and their BPF programs (bpf/)
nix/packages.nix  builds hanare and the static vhost-device-vsock
examples/         hanare.toml and a guest flake to start from
```

## Layout on the host

```
~/.local/src/hanare/      this repository
~/.local/libexec/hanare/  vhost-device-vsock (installed by "hanare update")
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
hanare update        # also installs vhost-device-vsock
hanare reinstall && hanare up
```

If you use a rootless Nix store, prefix the build output path with the
store root.

## Usage

```
hanare run [--offline] CMD...   run CMD in the VM, in the current directory
hanare shell                interactive shell in the VM
hanare up | down | status | logs
hanare audit [-f] [-n N]    guest activity log (JSON lines)
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

- **Writable shares are trusted input:** whatever the guest writes into a
  writable share is later read, and possibly executed, by host tools. That
  includes git hooks and `.git/config` (including bare-repo layouts), agent
  settings such as `.claude/settings.json`, `.mcp.json`, `.envrc`, build
  scripts and binaries.
  - With `hanare.protectShares` (the default), existing paths of that kind
    (`.git`, `.claude`, `.envrc`, `.vscode`, ...) are bind-mounted read-only
    inside the guest at boot, on `hanare share`, and every 30s.
  - Newly created ones are not blocked.
  - Neutralize such files on the host side, for example:
    - run git with `safe.bareRepository=explicit` (protected config) and a
      `core.hooksPath` you control;
    - start agents with project hooks disabled in shared directories;
    - use read-only shares where the guest only needs to read.
- **Linked tool names:** links in `~/.local/bin` lose to host commands that
  come earlier in `PATH`. `hanare link` skips those names.
- **Refused shares:** `hanare` will not share `$HOME` itself, `~/.ssh`,
  `~/.gnupg`, `~/.claude`, `~/.config` or `~/.local`, nor any directory
  that contains them.
