---
name: antic
description: >-
  Run CLI commands, builds, tests, and tools inside isolated NixOS microVMs using antic.
  Use this skill when executing commands in an isolated VM, installing Nix packages in a
  guest environment, sharing directories, forwarding ports, or when the user mentions antic.
---

# antic — Isolated NixOS Guest Runner

`antic` runs CLI commands inside lightweight, isolated NixOS virtual machines ("guests") backed by `cloud-hypervisor` and KVM. Host directories shared with a guest appear at the **exact same absolute path** inside the guest.

Every guest-targeted command requires the explicit guest `<name>` (omitting it is a syntax error). Run `antic guest ls` to see available guests.

## 1. Running Commands

Run any command inside a guest (automatically starts the guest if stopped):

```sh
# Run in the current directory (if shared) or guest ~
antic run <name> -- <command> [args...]

# Examples (using guest "dev")
antic run dev -- git status
antic run dev -- cargo test
antic run dev --offline -- pytest          # Run without network access inside the guest
antic run dev -w /tmp -- sh -c "uname -a"  # Explicit working directory in the guest
```

- Exit code `125` means an `antic` error occurred; any other exit code is the command's own exit status.
- Inside writable shares, sensitive paths (`.git`, `.claude`, `CLAUDE.md`, `AGENTS.md`, `.envrc`, `.vscode`, `mise.toml`, etc.) are automatically mounted **read-only** so processes inside the guest cannot tamper with git hooks or agent configs.

## 2. Installing Packages (`antic guest install`)

If a command or tool is missing inside the guest, install it from `nixpkgs`:

```sh
antic guest install <name> ripgrep jq nodejs python3
antic guest uninstall <name> nodejs
```

`antic guest install` updates `~/.config/antic/guests/<name>.toml`, builds the new NixOS closure on the host, copies it into the guest store, and live-switches the running guest without rebooting.

## 3. Sharing Directories (`antic guest mount`)

If the current working directory is not yet shared with the guest, mount it:

```sh
antic guest mount <name> ~/Work/my-project        # Read-write share
antic guest mount <name> ~/Docs/reference --ro    # Read-only share
antic guest unmount <name> ~/Docs/reference
```

`antic guest mount` persists the share in `~/.config/antic/guests/<name>.toml` and hot-plugs it into the running guest immediately.

## 4. Forwarding Ports (`antic guest forward`)

Expose a server running inside the guest on the host's `127.0.0.1`:

```sh
antic guest forward <name> 3000          # 127.0.0.1:3000 (host) -> 3000 (guest)
antic guest forward <name> 8080:80       # 127.0.0.1:8080 (host) -> 80 (guest)
antic guest unforward <name> 8080
```

## 5. Managing Guests & Resources (`antic guest`)

```sh
antic guest ls
antic guest show <name>                  # Status, resources, ports, and mounts (--config for raw TOML, --path for file path)

# Create a new guest (automatically builds and starts it)
antic guest create dev -p git -p ripgrep --cpus 4 --memory 8G --network internet

# Change vCPUs, RAM, or network mode ("internet" | "lan" | "none")
antic guest set <name> --cpus 4 --memory 8G --network internet

# Lifecycle & debugging
antic guest start <name>
antic guest stop <name>
antic guest restart <name>
antic guest ps <name>                    # List active command sessions
antic guest kill <name> <session-unit>   # Terminate hung session
antic guest logs <name>                  # Kernel/console log (--journal for systemd journal)
```

## 6. Configuration Files

Each guest is configured in its own TOML file at `~/.config/antic/guests/<name>.toml`:

```toml
description = "tools for my agent"
packages = ["git", "ripgrep"]

[spec]
cpus = 2
memory = "4G"

[network]
mode = "internet"   # none | internet | lan
# allow = ["github.com", "*.githubusercontent.com", "1.1.1.1/32"]  # Optional: default deny whitelist
# deny = ["10.0.0.0/8"]                 # Optional: blacklist (CIDR or domain)

[[network.ports]]
host = 8080
guest = 80
proto = "tcp"          # "tcp" (default) | "udp" | "tcp,udp"
description = "Web app"

[[shares]]
source = "~/Work"
mode = "rw"         # "rw" | "ro"

[env]
# EDITOR = "nvim"
```
