# hanare

hanare runs CLI tools, especially ones an AI agent drives, in NixOS virtual
machines ("guests") that are isolated from the host. Everything runs as your
own user; there is no root daemon. Host directories are shared at the same
path, and `hanare run CMD` runs CMD inside a guest, in your current
directory.

The guest side is **banken** (番犬, "watchdog"): an agent and BPF programs
that run inside each guest.

## How it works

- **VM:** [cloud-hypervisor] with KVM, booted directly from a kernel and
  initrd. The configuration, including credentials, goes through its API
  socket, never the command line.
- **Guest filesystem:** `/` is tmpfs. `/nix` is the guest's own Nix store,
  kept on the host and mounted read-only, so nothing in the guest survives a
  reboot and even guest root cannot change installed software.
  - `hanare guest apply` builds the guest's NixOS on the host, copies it into
    that store, and switches the running guest to it. The guest needs no
    network and evaluates nothing itself.
- **Shares:** one rootless [virtiofsd] per shared directory, mapping only
  your UID (as the guest user).
  - Inside writable shares, the host exports existing paths that host tools
    execute or trust read-only: `.git` (and bare-repository layouts),
    `.claude`, `.mcp.json`, `CLAUDE.md`, `AGENTS.md`, `.envrc`, `.vscode`,
    `.devcontainer`, `mise.toml`, and so on (`proto.ProtectedNames` plus
    `policy.protect`). The guest cannot change them, even as root. Newly
    created ones are not blocked.
  - Runtime shares are hot-plugged through cloud-hypervisor's API and
    mounted by banken.
- **Network:** [passt] (vhost-user) inside a rootless network namespace from
  pasta.
  - nftables there implements `network = "filtered"`: internet only, no
    LAN, CGNAT/Tailscale, link-local, host addresses or loopback.
  - `"lan"` still blocks the host and loopback; `"none"` gives the guest no
    network device at all.
  - The filter is outside the guest, so guest root cannot change it.
- **Access:** SSH over vsock. The host end is cloud-hypervisor's unix
  socket in a 0700 directory, so other host users cannot reach the guest.
  - The host generates and pins the guest's host key and passes it, with
    your public key (`restrict,pty`), as systemd credentials.
  - No passwords, sudo, setuid wrappers or root login.
  - Each `hanare run` is one sshd unit instance in the guest. Sessions:
    - wait for `banken-ready.target` (keys, shares, userns restriction,
      audit), so nothing runs without those;
    - run with `NoNewPrivileges`, `RestrictNamespaces` and a syscall filter;
    - with `--offline` (or `commands.offline`) also have no network: a
      private network namespace, only `AF_UNIX`, and no access to the
      resolver, nsncd, D-Bus or vsock.
- **banken's agent:** a root agent on vsock port 24 that accepts only the
  host (the guest kernel sets the peer CID). It mounts runtime shares (no
  symlink is ever followed: `openat2` + `fsmount`/`move_mount`), switches
  systems, shows the journal, and lists or stops sessions. The guest user
  cannot talk to it.
- **Guest kernel hardening:**
  - a BPF LSM program denies user namespaces to non-root (a common first
    step of kernel exploits);
  - `lockdown=integrity`, kernel modules locked after boot;
  - io_uring, unprivileged BPF and userfaultfd disabled.
- **Audit:** BPF logs the guest user's executions (argv captured at exec),
  connections and datagrams (inet, inet6, unix, vsock) and files opened for
  writing, with the session they belong to. banken writes the records to a
  serial port that cloud-hypervisor appends to a file on the host, one per
  boot. The guest can neither rewrite nor delete what was logged.
- **Host sandbox:** every unit sees only what it needs from `$HOME` and
  runs with private PIDs, IPC and /tmp, a read-only system and no new
  privileges. cloud-hypervisor also runs with seccomp, `AF_UNIX` only and no
  network.
  - Units: `hanare-vm@ID` (cloud-hypervisor), `hanare-net@ID`,
    `hanare-share@ID:TAG`, in `hanare-ID.slice`. Helpers are bound to the VM
    unit and stop with it.

## Repository

```
cmd/hanare/        host CLI
cmd/banken/        guest agent and BPF loaders (run by the guest's units)
internal/proto/    the host/guest contract (ports, credentials, agent protocol)
internal/host/     config, instance (IDs, directories, locks), systemd, chv
                   (cloud-hypervisor API), transport (vsock, SSH, agent),
                   nixbuild, share (policy, protected paths), guest, cli
internal/guest/    agent, userns and audit (with their BPF programs)
nix/module.nix     the guest NixOS module (nixosModules.guest)
nix/packages.nix   hanare, banken, static cloud-hypervisor
examples/          hanare.toml and a guest flake to start from
```

## Host layout

```
~/.config/hanare/hanare.toml        guests, policy
~/.local/state/hanare/
  guests/<ID>/store/nix             a guest's Nix store (its /nix)
  guests/<ID>/audit/*.jsonl         audit log, one file per boot
  guests/<ID>/log/console.log       console
  names/<NAME> -> ../guests/<ID>    name to ID (IDs are never reused)
  ssh/id_ed25519                    your client key
~/.local/libexec/hanare/            cloud-hypervisor ("hanare upgrade")
$XDG_RUNTIME_DIR/hanare/<ID>/       sockets, boot record
```

Keep guest flakes and this repository outside shared directories: a guest
that could write them would control what the host builds. hanare refuses to
share hidden directories of `$HOME` (and what symlinks there point to),
`$PATH` directories, its own directories and guest flakes.

## Requirements

- Linux with KVM and systemd ≥ 257 (user units with `PrivatePIDs`).
- Arch packages: `virtiofsd passt nftables`.
- Nix with flakes (it can be a rootless static `nix` with
  `store = local?root=$HOME/.local/share/nix-root`).

## Setup

```sh
git clone https://github.com/RNG999/hanare ~/.local/src/hanare
mkdir -p ~/.config/hanare ~/.local/state/hanare/ssh
cp ~/.local/src/hanare/examples/hanare.toml ~/.config/hanare/
cp -r ~/.local/src/hanare/examples/guest ~/.config/hanare/guest   # set hanare.user
ssh-keygen -t ed25519 -N '' -C hanare -f ~/.local/state/hanare/ssh/id_ed25519
nix run path:$HOME/.local/src/hanare -- upgrade   # installs hanare and cloud-hypervisor
hanare doctor
hanare guest create dev
hanare run uname -a
```

## Usage

```
hanare run [--offline] [-t|-T] [--] CMD...   run CMD in the guest, in the current directory
hanare shell
hanare audit [-f] [-n N] [--all] [--session S] [--json]

hanare guest ls [--json] [-q]
hanare guest create NAME [--from GUEST | --flake F --attr A]
hanare guest rm NAME... [-y] [--keep-config]
hanare guest start|restart [NAME...]
hanare guest stop NAME... | --all
hanare guest inspect [NAME] [--json]
hanare guest logs [NAME] [-f] [--journal [-u UNIT] [-n N]]
hanare guest apply [NAME]
hanare guest prune [-y]

hanare session ls [--json] | kill SESSION...
hanare share ls [--json] | add [--ro] [--persist] [-y] DIR... | rm [--persist] DIR...
hanare link ls [--json] | sync | add TOOL... | rm TOOL...
hanare config path | check | edit
hanare doctor | upgrade [--source DIR] | version | completion SHELL
```

The guest is chosen by an argument (guest subcommands), `-g NAME`,
`$HANARE_GUEST`, `default_guest`, or is the only one defined. `run` exits
with the command's status; hanare's own errors exit with 125.

Add tools to `environment.systemPackages` in your guest flake, then run
`hanare guest apply`. A changed kernel needs `hanare guest restart`.

`apply` refuses to build if the guest flake's lock pins an older state of a
local-path input, or if a local input lies inside a share.

## Caveats

- **Writable shares are trusted input.** Whatever the guest writes into a
  writable share may later be read or executed on the host: new
  `.git`/`.claude`/`.envrc`, build scripts, binaries. Review changes, keep
  host tools from auto-trusting workspaces, and use read-only shares where
  the guest only needs to read.
- **Network egress is open in `filtered` mode** (internet only); shares
  can be exfiltrated. Use `--offline`, `commands.offline` or
  `network = "none"` for tools that need no network.
- **Audit is a record, not a barrier,** and the guest kernel produces it:
  guest root could stop it (from then on, not retroactively).
- **Links in `~/.local/bin`** lose to host commands earlier in `PATH`;
  `hanare link ls` reports those as shadowed.

[cloud-hypervisor]: https://www.cloudhypervisor.org/
[virtiofsd]: https://gitlab.com/virtio-fs/virtiofsd
[passt]: https://passt.top/
