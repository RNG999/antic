# antic

`antic` runs CLI tools—especially those driven by AI coding agents—inside lightweight, isolated NixOS microVMs ("guests") powered by [cloud-hypervisor] and KVM.

Everything runs rootless as your own user with no privileged daemon. Host directories shared with a guest appear at the **exact same path** inside the VM, so `antic run -- <cmd>` executes seamlessly in your current working directory.

---

## Requirements

- **Linux** (`x86_64` or `aarch64`) with KVM (`/dev/kvm`) and **systemd ≥ 257** (user units with `PrivatePIDs`).
- **Nix** with flakes.

`antic update` installs `antic` (`~/.local/bin/antic`) along with `cloud-hypervisor`, `virtiofsd`, `passt`/`pasta`, and `nft` into `~/.local/libexec/antic/` from `nixpkgs`, so no extra virtualization or firewall packages need to be installed on the host.

## Setup & Quickstart

Run the installer script to ensure Nix is configured, build and install `antic` and its helper binaries, and verify the setup:

```sh
curl -fsSL https://raw.githubusercontent.com/RNG999/antic/main/install.sh | sh
```

Or manually with `nix run`:

```sh
nix --extra-experimental-features "nix-command flakes" run github:RNG999/antic -- update
antic doctor
```

Then create a guest and run commands inside it:

```sh
# Create and start a guest named "dev" with git and ripgrep installed
antic guest create dev -p git -p ripgrep

# Share a workspace directory and run a command inside the guest
antic guest mount dev ~/Work
cd ~/Work
antic run dev -- uname -a
```

> [!TIP]
> When only **one** guest is defined, the `[NAME]` argument is optional across commands (e.g. `antic run -- cargo test`, `antic shell`, `antic guest install jq`).

---

## Usage

### Running Commands & Shells

```sh
antic run [NAME] [-w DIR] [-t|-T] [--] CMD [ARG...]   # Run CMD in the guest (starts it if stopped)
antic shell [NAME]                                    # Open an interactive login shell
```

- Commands run in your current directory if it lies inside a shared path, or in the guest's `~` otherwise (override with `-w DIR`).
- `antic run` exits with the command's own exit code (`antic`'s own errors exit with `125`).

### Managing Packages, Shares, and Ports

```sh
# Add or remove nixpkgs packages and live-switch the guest without rebooting
antic guest install [NAME] PKG...
antic guest uninstall [NAME] PKG...

# Hot-plug or remove shared directories (persisted in the guest's TOML config)
antic guest mount [NAME] [--ro] DIR...
antic guest unmount [NAME] DIR...

# Forward host ports (127.0.0.1:HOST -> GUEST)
antic guest forward [NAME] PORT... [-p tcp|udp|tcp,udp] [-d DESC]   # e.g. 3000, 8080:80, 53/tcp,udp
antic guest unforward [NAME] PORT...
```

### Guest Lifecycle & Inspection

```sh
antic guest ls [--json] [-q]
antic guest show [NAME] [--path] [--config] [--json]
antic guest stats [NAME] [-w] [--json]                # Live CPU, memory, and session metrics

antic guest create NAME [-p PKG...] [--cpus N] [--memory SIZE] [--network MODE] [--port PORT...]
antic guest set [NAME] [--cpus N] [--memory SIZE] [--network MODE]
antic guest edit [NAME]                               # Edit TOML config in $EDITOR and validate
antic guest apply [NAME]                              # Rebuild NixOS closure and switch running guest

antic guest start [NAME...]
antic guest stop [NAME...] | --all
antic guest restart [NAME...]
antic guest rm NAME... [-y]

antic guest ps [NAME] [--json]                        # List active command sessions
antic guest kill [NAME] SESSION...                    # Stop a running session unit
antic guest logs [NAME] [-f] [--journal [-u UNIT] [-n N]]
```

### Cache, Claude Code Skill & Host Maintenance

```sh
antic cache ls [--json]                               # List cached guest builds
antic cache rm [GUEST...] [--all]                     # Remove unused (or specified) caches and run Nix GC

antic skill install [-p]                              # Install SKILL.md (~/.claude/skills/antic or ./.claude/skills/antic)
antic skill rm [-p]
antic skill path [-p]

antic doctor                                          # Check KVM, helper binaries, Nix, and guest configs
antic update [--source DIR]                           # Build and install antic and helper binaries
antic version
antic completion bash|zsh|fish
```

---

## Configuration

### Global Guest Config (`~/.config/antic/guests/<name>.toml`)

Each guest is configured in its own TOML file:

```toml
description = "Development environment"
packages = ["git", "curl", "ripgrep"]

[spec]
cpus = 4
memory = "8G"

[network]
mode = "internet"   # "internet" (default) | "lan" | "none"
# default = "deny"  # "allow" (default) | "deny": what destinations no allow entry names get
# allow = ["github.com", "*.githubusercontent.com", "1.1.1.1/32"]
# deny = ["10.0.0.0/8"]

[[network.ports]]
host = 8080
guest = 80
proto = "tcp"       # "tcp" (default) | "udp" | "tcp,udp"
description = "Web server"

[[shares]]
source = "~/Work"
mode = "rw"         # "rw" (default) | "ro"

[env]
EDITOR = "nvim"
```

### Project-Local Config (`antic.toml`)

You can place an `antic.toml` file at the root of a project repository. When you run `antic` inside that directory (or any subdirectory), it automatically discovers the project guest:

```toml
name = "my-project"
description = "Project-specific guest environment"
flake = "nix/guest#my-project"   # or: packages = ["git", "nodejs"]

[spec]
cpus = 4
memory = "4G"

[network]
mode = "internet"

[[shares]]
source = "."
mode = "rw"
```

In `antic.toml`, `name` is required, and relative paths in `flake` and `[[shares]]` are resolved relative to the directory containing `antic.toml`.

### Custom NixOS Flakes

For full NixOS configuration beyond a package list, set `flake = "/path/to/flake#attr"` instead of `packages = [...]` and import `antic.nixosModules.guest` (see [`examples/guest`](examples/guest)):

```nix
{
  inputs = {
    antic.url = "github:RNG999/antic";
    nixpkgs.follows = "antic/nixpkgs";
  };

  outputs = { antic, nixpkgs, ... }: {
    nixosConfigurations.antic = nixpkgs.lib.nixosSystem {
      system = "x86_64-linux";
      modules = [
        antic.nixosModules.guest
        ({ pkgs, ... }: {
          antic.user = "your-host-username";
          environment.systemPackages = with pkgs; [ git ];
        })
      ];
    };
  };
}
```

---

## Architecture & Security Model

- **Stateless microVM:** Boots directly from a kernel and initrd via `cloud-hypervisor`. `/` is `tmpfs` (reset on every boot) and `/nix` is a per-guest store (`~/.local/state/antic/guests/<ID>/store/nix`) mounted read-only over `virtiofs`. The guest has no Nix daemon, no bootloader, no SSH daemon, no `sudo`, and no setuid wrappers.
- **Live switching with rollback:** `antic guest apply` builds the NixOS system on the host, copies the closure into the guest store, and switches the running guest over vsock. If switching fails, it automatically rolls back to the previous generation.
- **Protected paths in writable shares:** Each share runs an isolated rootless `virtiofsd` mapping only your UID/GID. Inside writable shares, sensitive files and directories that already exist on the host—such as `.git` (including bare repos), `.claude`, `.mcp.json`, `CLAUDE.md`, `AGENTS.md`, `.envrc`, `.vscode`, `.devcontainer`, `mise.toml`, etc.—are bound **read-only** by the host so even guest `root` cannot modify them.
- **Host-side egress filtering:** Networking runs via [passt] inside a rootless `pasta` network namespace. Firewall rules (`nftables`) and DNS filtering live outside the guest:
  - `"internet"` (default): allows internet access while blocking LAN subnets, CGNAT/Tailscale (`100.64.0.0/10`), link-local/cloud metadata (`169.254.0.0/16`), and host loopback.
  - `"lan"`: allows internet and LAN, while still blocking host addresses, loopback, and link-local.
  - `"none"`: attaches no network device to the VM.
  - `default`, `allow` and `deny` refine the mode independently:
    - `deny` entries (domains, including `*.example.com` wildcards, and CIDRs) are never reachable.
    - `allow` CIDRs are always reachable, even inside the LAN in `"internet"` mode.
    - `default = "allow"` (the default) reaches the mode's base set. `default = "deny"` reaches only `allow` CIDRs and the addresses `allow` domains resolve to: an in-namespace DNS proxy resolves allowed domains and adds their addresses to an `nftables` set before answering, and refuses every other name. Resolved addresses never open the LAN in `"internet"` mode.
    - An `allow` list requires an explicit `default` (older configs where `allow` implied "deny everything else" must add `default = "deny"`), and `allow` domains require `default = "deny"`.
- **Host sandboxing:** Every host helper unit (`antic-vm@`, `antic-net@`, `antic-share@`) runs under `systemd --user` with `ProtectHome=tmpfs`, `ProtectSystem=strict`, `PrivateTmp`, `PrivatePIDs`, and explicit bind mounts for only the paths it needs.

### Caveats

- **Writable shares are untrusted output:** Files created by the guest inside a writable share (new build scripts, binaries, or newly created dotfiles) remain on the host filesystem. Review changes before executing them on the host, and use `--ro` (`mode = "ro"`) where possible.
- **Guest flakes must live outside writable shares:** `antic` refuses to build a local flake input located inside a shared directory, and refuses to share hidden directories in `$HOME` or `$PATH` directories.
- **Shared CDN IPs with `allow`:** Because `allow` rules permit resolved IP addresses in `nftables`, other domains hosted behind the same shared CDN IP addresses become reachable at the IP layer while the entry is active.

## Development & Verification

The top-level behavior is a state-transition specification (`internal/spec`, see `docs/design/spec.md`) that serves as the test oracle; the command saga is model-checked (`formal/`). The tasks are in the `justfile` (`just` lists them). Install `just` on the host: a rootless Nix cannot start `nix develop` from inside another Nix shell, and the E2E tests must run outside it.

```bash
just check              # every change: nix flake check (unit tests, property-based tests, Z3, TLC, Spacer) and lint
just e2e run            # this host: every E2E test against real antic, with saga traces validated against the TLA+ model
just e2e run 'TestE2E$/sessions'   # one test (a -test.run filter)
just test pbt 20000     # property-based tests with more cases
just nightly            # larger property-based tests, then every E2E test with generated command sequences
```

The E2E tests run antic for real (KVM, `systemd --user`) and only touch guests named `e2e-*`; run them from a plain shell, not inside `nix develop`.

[cloud-hypervisor]: https://www.cloudhypervisor.org/
[virtiofsd]: https://gitlab.com/virtio-fs/virtiofsd
[passt]: https://passt.top/
