# antic

antic runs CLI tools, especially ones an AI agent drives, in minimal NixOS
virtual machines ("guests") that are isolated from the host. Everything runs as
your own user; there is no root daemon. Host directories are shared at the same
path, and `antic run CMD` runs CMD inside a guest, in your current directory.

The guest side is **antic-agent**: a lightweight, pure-Go static binary that
runs inside each guest to handle command sessions and host agent requests.

## How it works

- **VM:** [cloud-hypervisor] with KVM, booted directly from a kernel and
  initrd. The configuration, including credentials, goes through its API
  socket, never the command line.
- **Guest filesystem:** `/` is tmpfs. `/nix` is the guest's own Nix store,
  kept on the host and mounted read-only, so nothing in the guest survives a
  reboot and even guest root cannot change installed software.
  - The guest uses NixOS's `minimal` profile with Rust `nixos-init` (no
    bootloader, desktop/XDG assets, LVM, bcache, FUSE or OpenSSH).
  - `antic guest apply` builds the guest's NixOS on the host, copies it into
    that store, and switches the running guest to it. The guest needs no
    network and evaluates nothing itself. If switching fails, it automatically
    rolls back the running guest to the previous configuration.
- **Shares:** one rootless [virtiofsd] per shared directory, mapping only
  your UID (as the guest user).
  - Inside writable shares, the host exports existing paths that host tools
    execute or trust read-only: `.git` (and bare-repository layouts),
    `.claude`, `.mcp.json`, `CLAUDE.md`, `AGENTS.md`, `.envrc`, `.vscode`,
    `.devcontainer`, `mise.toml`, and so on (`proto.ProtectedNames` plus
    `policy.protect`). The guest cannot change them, even as root. Newly
    created ones are not blocked.
  - Runtime shares are hot-plugged through cloud-hypervisor's API and
    mounted by `antic-agent`.
- **Network:** [passt] (vhost-user) inside a rootless network namespace from
  pasta.
  - nftables there implements `[network]`: `mode = "internet"` (the default) allows
    internet only, blocking LAN (including the host's on-link subnets, IPv4 and IPv6),
    CGNAT/Tailscale, link-local/metadata, host addresses or loopback.
  - `"lan"` still blocks the host, link-local and loopback; `"none"` gives the
    guest no network device at all.
  - `allow = [...]` (domains and CIDRs) turns the filter into default deny, while
    `deny = [...]` blocks specific domains and CIDRs.
    antic runs a small in-process DNS filter in the namespace: it answers only
    A/AAAA queries for non-denied/allowed domains (and their subdomains), refuses
    everything else, and adds the answer's addresses to an nftables set *before*
    the guest sees them. Connecting by IP without asking DNS is rejected; only the
    filter's marked sockets can reach the upstream resolver. In `"internet"`
    mode, DNS answers pointing into the LAN stay blocked (static CIDR entries
    may name LAN hosts).
  - The filter is outside the guest, so guest root cannot change it.
- **Access:** direct vsock sessions via `antic-agent session` (no `sshd`, SSH
  keys, passwords, PAM, sudo or setuid wrappers). The host end is
  cloud-hypervisor's unix socket in a 0700 directory, and the guest checks that
  the peer CID is the host.
  - Each `antic run` is one socket-activated `antic-session@` unit instance in
    the guest. Sessions:
    - wait for `antic-ready.target` (configured shares), so nothing runs
      without those;
    - run directly as the guest user with empty `CapabilityBoundingSet`,
      `NoNewPrivileges`, `RestrictNamespaces` and a syscall filter;
    - with `--offline` (or `commands.offline`) also have no network: a
      private network namespace, only `AF_UNIX`, and no access to the
      resolver, nsncd, D-Bus or vsock.
- **antic-agent daemon:** a root agent on vsock port 24 that accepts only the
  host (the guest kernel sets the peer CID). It mounts runtime shares (no
  symlink is ever followed: `openat2` + `fsmount`/`move_mount`), switches
  systems, shows the journal, and lists or stops sessions. The guest user
  cannot talk to it.
- **Guest kernel hardening:**
  - `lockdown=integrity`, kernel modules locked after boot;
  - io_uring, unprivileged BPF and userfaultfd disabled.
- **Host sandbox:** every unit sees only what it needs from `$HOME` and
  runs with private PIDs, IPC and /tmp, a read-only system and no new
  privileges. cloud-hypervisor also runs with seccomp, `AF_UNIX` only and no
  network.
  - Units: `antic-vm@ID` (cloud-hypervisor), `antic-net@ID`,
    `antic-share@ID:TAG`, in `antic-ID.slice`. Helpers are bound to the VM
    unit and stop with it.

## Repository

```
cmd/antic/         host CLI
cmd/antic-agent/   guest agent and session runner (run by the guest's units)
internal/proto/    the host/guest contract (ports, credentials, session & agent protocols)
internal/host/     config, instance (IDs, directories, locks), systemd, chv
                   (cloud-hypervisor API), transport (vsock, session, agent),
                   nixbuild, share (policy, protected paths), guest, cli
internal/guest/    agent and session
nix/module.nix     the guest NixOS module (nixosModules.guest)
nix/packages.nix   antic, antic-agent, static cloud-hypervisor
examples/          dev.toml and a guest flake to start from
```

## Host layout

```
~/.config/antic/guests/<NAME>.toml  one config file per guest
~/.local/state/antic/
  guests/<ID>/store/nix             a guest's Nix store (its /nix)
  guests/<ID>/log/console.log       console
  names/<NAME> -> ../guests/<ID>    name to ID (IDs are never reused)
  locks/<NAME>.lock                 guest lifecycle and build locks (flock with timeout and holder PID)
~/.local/libexec/antic/             cloud-hypervisor ("antic update")
$XDG_RUNTIME_DIR/antic/<ID>/        sockets, boot record
```

Keep guest flakes and this repository outside shared directories: a guest
that could write them would control what the host builds. antic refuses to
share hidden directories of `$HOME` (and what symlinks there point to),
`$PATH` directories, its own directories and guest flakes.

## Requirements

- Linux with KVM and systemd ≥ 257 (user units with `PrivatePIDs`).
- Arch packages: `virtiofsd passt nftables`.
- Nix with flakes (it can be a rootless static `nix` with
  `store = local?root=$HOME/.local/share/nix-root`).

## Setup

```sh
mkdir -p ~/.config/antic/guests ~/.local/state/antic ~/Work
nix run github:RNG999/antic -- update   # installs antic and cloud-hypervisor
antic doctor
antic guest create dev -p git
antic run dev -- uname -a
```

## Guest configuration

Guest definitions live in `~/.config/antic/guests/<NAME>.toml`:

```toml
description = "tools for my agent"
packages = ["git", "curl"]

[spec]
cpus = 4
memory = "8G"

[network]
mode = "internet"   # "internet" | "lan" | "none"
# allow = ["github.com", "*.githubusercontent.com", "1.1.1.1/32"]
# deny = ["10.0.0.0/8"]

[[network.ports]]
host = 8080
guest = 80
proto = "tcp"          # "tcp" (default) | "udp" | "tcp,udp"
description = "Web app"

[[shares]]
source = "~/Work"
mode = "rw"            # "rw" | "ro"

[env]
EDITOR = "nvim"
```

## Usage

```
antic run NAME [--offline] [-w DIR] [-t|-T] [--] CMD...   run CMD in the guest
antic shell NAME

antic guest ls [--json] [-q]
antic guest create NAME [-p PKG...] [--cpus N] [--memory SIZE] [--network MODE] [--port PORT...]
antic guest set NAME [--cpus N] [--memory SIZE] [--network MODE]
antic guest edit NAME
antic guest rm NAME... [-y]
antic guest start|stop|restart NAME...
antic guest show [NAME] [--path] [--config] [--json]
antic guest logs NAME [-f] [--journal [-u UNIT] [-n N]]
antic guest ps NAME [--json]
antic guest kill NAME SESSION...
antic guest apply NAME

antic guest install NAME PKG... | uninstall NAME PKG...
antic guest mount NAME [--ro] DIR... | unmount NAME DIR...
antic guest forward NAME PORT... [-p tcp|udp|tcp,udp] [-d DESC] | unforward NAME PORT...
antic skill install [-p] | rm [-p] | path [-p]
antic doctor | update [--source DIR] | version | completion SHELL
```

Every guest-targeted command requires an explicit `NAME` argument (omitting it is a syntax error). `run` exits with the command's status; antic's own errors exit with 125.

Add tools with `antic guest install NAME PKG...` (or edit `packages = [...]` in
`~/.config/antic/guests/<name>.toml` and run `antic guest apply NAME`). A changed
kernel needs `antic guest restart NAME`.

`apply` refuses to build if the guest flake's lock pins an older state of a
local-path input, or if a local input lies inside a share.

## Caveats

- **Writable shares are trusted input.** Whatever the guest writes into a
  writable share may later be read or executed on the host: new
  `.git`/`.claude`/`.envrc`, build scripts, binaries. Review changes, keep
  host tools from auto-trusting workspaces, and use read-only shares where
  the guest only needs to read.
- **Network egress is open in `internet` mode**; shares can be exfiltrated.
  Use `[network]` with `allow = [...]`, `deny = [...]`, `--offline` or
  `mode = "none"` for tools that need little or no network.
- **`allow` is enforced by IP address.** An allowed domain's addresses are
  often shared CDN addresses (Cloudflare, Fastly, ...), so other sites on the
  same addresses become reachable too, and data can be sent to anything the
  allowed services accept (e.g. a gist on github.com). Allow only what is
  needed.
- **Links in `~/.local/bin`** lose to host commands earlier in `PATH`;
  `antic link ls` reports those as shadowed.

[cloud-hypervisor]: https://www.cloudhypervisor.org/
[virtiofsd]: https://gitlab.com/virtio-fs/virtiofsd
[passt]: https://passt.top/
