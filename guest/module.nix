# Guest side of hanare. Import this module (nixosModules.default) from your
# own flake and set at least `hanare.user`; see README.md.
{ config, lib, modulesPath, pkgs, ... }:

let
  cfg = config.hanare;
  hanare = (import ../nix/packages.nix { inherit pkgs; }).hanare;
  user = cfg.user;
  home = "/home/${user}";
  protect = cfg.protectShares;

  # Paths inside shares that host-side tools execute or trust as configuration
  # (git, Claude Code, Codex, Gemini CLI, Cursor, VS Code, direnv, mise, ...).
  # A guest that could write them would get code run on the host.
  protectedNames = [
    ".git" ".claude" ".codex" ".gemini" ".cursor" ".vscode" ".idea" ".devcontainer"
    ".husky" ".githooks" ".mise" ".mcp.json" "CLAUDE.md" "CLAUDE.local.md" "AGENTS.md"
    "GEMINI.md" ".cursorrules" ".envrc" "mise.toml" ".mise.toml"
  ];
  namesRegex = lib.concatMapStringsSep "|" lib.escapeRegex protectedNames;
  guardPath = lib.makeBinPath (with pkgs; [ util-linux findutils gnugrep gnused coreutils ]);

  # Bind-mounts existing protected paths read-only (root-owned mounts, so
  # unprivileged guest processes cannot lift them), including bare-repository
  # layouts and the targets of ".git" gitdir files.
  # Usage: hanare-guard [DIR...] (default: all writable shares).
  shareGuard = pkgs.writeShellScript "hanare-guard" ''
    set -uo pipefail
    export PATH=${guardPath}
    roots=("$@")
    if (( ''${#roots[@]} == 0 )); then
      mapfile -t roots < <(findmnt -rn -t virtiofs -O rw -o TARGET | grep '^${home}/')
    fi
    protect() {
      [[ -e $1 ]] || return 0
      findmnt -rn --mountpoint "$1" -O ro >/dev/null && return 0
      mount --bind -- "$1" "$1" && mount -o remount,bind,ro -- "$1"
    }
    for root in "''${roots[@]}"; do
      while IFS= read -r -d "" p; do
        protect "$p"
        # Worktrees/submodules: ".git" is a file pointing at the real gitdir.
        if [[ -f $p && ''${p##*/} == .git ]]; then
          target=$(sed -n 's/^gitdir: //p' "$p" | head -1)
          [[ $target == /* ]] || target=$(dirname "$p")/$target
          target=$(realpath -m -- "$target")
          [[ $target == "$root"/* ]] && protect "$target"
        fi
      done < <(find "$root" -xdev -maxdepth 6 -name node_modules -prune -o \
                 -regextype posix-extended -regex '.*/(${namesRegex})' -print0 -prune)
      # Bare-repository layouts outside .git (git would treat them as repos).
      while IFS= read -r -d "" head; do
        d=$(dirname "$head")
        [[ -d $d/objects && -d $d/refs ]] && protect "$d"
      done < <(find "$root" -xdev -maxdepth 6 \( -name node_modules -o -name .git \) -prune -o \
                 -type f -name HEAD -print0)
    done
  '';

  # sshd for one vsock connection (see the hanare-ssh sockets).
  sshdService = {
    ExecStart = "-${config.services.openssh.package}/bin/sshd -i -f /etc/ssh/sshd_config -o UsePAM=no";
    StandardInput = "socket";
    StandardError = "journal";
  };

  # Mounts a hot-plugged share; instance = systemd-escape "TAG|ro|PATH".
  # Only tags made by the host and mount points the user owns below its home
  # are accepted, so the user cannot mount host content over system paths.
  hanareMount = pkgs.writeShellScript "hanare-mount" ''
    set -euo pipefail
    IFS='|' read -r tag mode path <<<"$(${pkgs.systemd}/bin/systemd-escape --unescape -- "$2")"
    [[ $tag =~ ^s-[0-9a-f]{12}$ && ($mode == ro || $mode == rw) ]] || { echo "bad instance" >&2; exit 1; }
    real=$(${pkgs.coreutils}/bin/realpath -e -- "$path")
    [[ $real == ${home}/* && $(${pkgs.coreutils}/bin/stat -c %U -- "$real") == ${user} ]] \
      || { echo "refusing mount point $real" >&2; exit 1; }
    case $1 in
      start)
        ${pkgs.util-linux}/bin/mount -t virtiofs -o "$mode,nosuid,nodev" -- "$tag" "$real"
        ${lib.optionalString protect ''
          if [[ $mode == rw ]]; then
            ${shareGuard} "$real"
          fi
        ''}
        ;;
      stop) exec ${pkgs.util-linux}/bin/umount -R -- "$real" ;;
    esac
  '';
in
{
  imports = [ "${modulesPath}/profiles/qemu-guest.nix" ];

  options.hanare = {
    user = lib.mkOption {
      type = lib.types.str;
      description = "Guest login user; must be the host user that runs hanare (UID/GID 1000 in the guest).";
    };
    restrictUserNamespaces = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Deny creating user namespaces to everyone but root (BPF LSM), a common
        first step of kernel exploits. Root, e.g. the nix-daemon build
        sandbox, is unaffected.
      '';
    };
    audit = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Log program executions, outgoing connections and files opened for
        writing by the guest user (BPF), per session; see "hanare audit".
      '';
    };
    protectShares = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Keep existing host-executed configuration in writable shares (.git,
        .claude, .mcp.json, .envrc, .vscode, ...) read-only in the guest.
        Newly created paths are not blocked.
      '';
    };
  };

  config = {

    system.stateVersion = "26.05";
    networking.hostName = "hanare";

    boot.loader.systemd-boot.enable = true;
    boot.loader.efi.canTouchEfiVariables = false;
    boot.initrd.systemd.enable = true;
    # systemd.ssh_auto=0: keep systemd-ssh-generator from creating its own vsock/AF_UNIX sshd sockets.
    boot.kernelParams = [ "console=hvc0" "systemd.ssh_auto=0" ];
    # dmi_sysfs: lets systemd read credentials (fstab.extra = shares) from SMBIOS.
    boot.initrd.kernelModules = [ "dmi_sysfs" ];
    boot.kernelModules = [ "vmw_vsock_virtio_transport" ];
    # The stock growpart targets "/", which is tmpfs here; grow /persist instead
    # (the image is truncated to vm.disk_size in hanare.toml by "hanare reinstall").
    boot.growPartition = false;
    systemd.services.hanare-growpart = {
      wantedBy = [ "multi-user.target" ];
      after = [ "persist.mount" ];
      serviceConfig.Type = "oneshot";
      path = with pkgs; [ cloud-utils.guest util-linux e2fsprogs gawk gnused ];
      script = ''
        part=$(readlink -f /dev/disk/by-label/nixos)
        disk=/dev/$(lsblk -no PKNAME "$part")
        growpart "$disk" "$(cat /sys/class/block/''${part#/dev/}/partition)" || true
        resize2fs "$part"
      '';
    };

    # Ephemeral root: everything except /nix and /persist is reset on every boot.
    # The disk's ext4 (label "nixos", the whole image) is mounted at /persist.
    fileSystems = {
      "/" = lib.mkForce {
        device = "none";
        fsType = "tmpfs";
        options = [ "mode=755" "size=50%" ];
      };
      "/persist" = {
        device = "/dev/disk/by-label/nixos";
        fsType = "ext4";
        autoResize = true;
        neededForBoot = true;
      };
      "/nix" = {
        device = "/persist/nix";
        fsType = "none";
        options = [ "bind" ];
        neededForBoot = true;
      };
      "/boot" = {
        device = "/dev/disk/by-label/ESP";
        fsType = "vfat";
        options = [ "umask=0077" ];
      };
    };
    systemd.tmpfiles.rules = [ "z ${home} 0700 ${user} ${user} -" ];

    # Less kernel attack surface and information for unprivileged processes.
    boot.kernel.sysctl = {
      "kernel.io_uring_disabled" = 2;
      "kernel.unprivileged_bpf_disabled" = 1;
      "kernel.perf_event_paranoid" = 2;
      "kernel.kptr_restrict" = 2;
      "kernel.dmesg_restrict" = 1;
    };

    # passt (user-mode networking): plain DHCP on the virtio NIC.
    networking.useNetworkd = true;
    networking.useDHCP = true;
    # LAN/loopback egress is filtered on the host (network.allow_lan in
    # hanare.toml), where guest root cannot change it.
    networking.nftables.enable = true;
    networking.firewall.enable = true;

    # Only key-based logins over vsock; no passwords, no sudo.
    users.mutableUsers = false;
    users.users.root.hashedPassword = "!";
    users.users.${user} = {
      isNormalUser = true;
      uid = 1000;
      # Primary GID must match the host (1000): virtiofsd only maps 1000:1000.
      group = user;
      # Lets "hanare switch" stream the rebuild log.
      extraGroups = [ "systemd-journal" ];
      # "*" rather than "!": without PAM, OpenSSH refuses "!"-locked accounts.
      hashedPassword = "*";
    };
    users.groups.${user}.gid = 1000;
    security.sudo.enable = false;
    users.allowNoPasswordLogin = true;

    # SSH only over vsock (port 22, socket-activated per connection); no TCP listener.
    services.openssh = {
      enable = true;
      openFirewall = false;
      listenAddresses = [ { addr = "127.0.0.1"; port = 22; } ];
      # Both keys come from the host as credentials (see hanare-ssh-keys):
      # the host pins the host key it generated, and the flake holds no key.
      hostKeys = [ ];
      authorizedKeysFiles = [ "/run/hanare/authorized_keys" ];
      extraConfig = "HostKey /run/hanare/ssh_host_ed25519_key";
      settings = {
        PermitRootLogin = "no";
        PasswordAuthentication = false;
        KbdInteractiveAuthentication = false;
        AllowUsers = [ user ];
        DisableForwarding = true;
        PermitTunnel = "no";
        MaxAuthTries = 2;
      };
    };
    systemd.services.sshd.enable = false;
    systemd.services.hanare-ssh-keys = {
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ImportCredential = [ "hanare.ssh.authorized_keys" "hanare.ssh.host_key" ];
        RuntimeDirectory = "hanare";
        RuntimeDirectoryPreserve = true;
      };
      # "restrict,pty": no forwarding of any kind, only a terminal.
      script = ''
        ${pkgs.gnused}/bin/sed 's/^/restrict,pty /' "$CREDENTIALS_DIRECTORY/hanare.ssh.authorized_keys" > /run/hanare/authorized_keys
        chmod 0644 /run/hanare/authorized_keys
        install -m 0600 "$CREDENTIALS_DIRECTORY/hanare.ssh.host_key" /run/hanare/ssh_host_ed25519_key
      '';
    };
    # Port 22: normal sessions; port 23: offline sessions ("hanare run
    # --offline"). sshd runs without PAM, so a session's processes stay in
    # the per-connection unit's cgroup and its restrictions apply to them.
    systemd.sockets.hanare-ssh = {
      wantedBy = [ "sockets.target" ];
      socketConfig = {
        ListenStream = "vsock::22";
        Accept = true;
      };
    };
    systemd.sockets.hanare-ssh-offline = {
      wantedBy = [ "sockets.target" ];
      socketConfig = {
        ListenStream = "vsock::23";
        Accept = true;
      };
    };
    # No ordering on hanare-ssh-keys: with Accept=yes it would put the socket
    # in an ordering cycle. The keys exist long before the host connects, and
    # hanare retries until SSH succeeds.
    systemd.services."hanare-ssh@".serviceConfig = sshdService;
    systemd.services."hanare-ssh-offline@".serviceConfig = sshdService // {
      # No IP at all (cgroup BPF), and no other way out: the resolver (also
      # via nsncd, NSS's caching daemon), D-Bus and the nix-daemon (which
      # fetches on a client's behalf) are hidden.
      PrivateNetwork = true;
      IPAddressDeny = "any";
      InaccessiblePaths = [
        "-/run/nscd"
        "-/run/systemd/resolve"
        "-/run/dbus/system_bus_socket"
        "-/nix/var/nix/daemon-socket"
      ];
    };

    # Shares: configured ones arrive as the fstab.extra credential at boot,
    # runtime ones are mounted by this unit (started by the user via polkit).
    systemd.services."hanare-mount@" = {
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${hanareMount} start %i";
        ExecStop = "${hanareMount} stop %i";
      };
    };

    # hanare.protectShares: guard existing paths once the boot-time shares are
    # mounted and every 30s after.
    systemd.services.hanare-guard = lib.mkIf protect {
      wantedBy = [ "multi-user.target" ];
      after = [ "local-fs.target" "remote-fs.target" ];
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${shareGuard}";
      };
    };
    systemd.timers.hanare-guard = lib.mkIf protect {
      wantedBy = [ "timers.target" ];
      timerConfig = {
        OnActiveSec = "30s";
        OnUnitActiveSec = "30s";
        AccuracySec = "5s";
      };
    };
    # hanare.restrictUserNamespaces / hanare.audit: BPF programs loaded by
    # "hanare guest ...", before user sessions so no command runs without them.
    systemd.services.hanare-userns = lib.mkIf cfg.restrictUserNamespaces {
      wantedBy = [ "multi-user.target" ];
      before = [ "systemd-user-sessions.service" ];
      after = [ "sys-fs-bpf.mount" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${hanare}/bin/hanare guest restrict-userns";
        ExecStop = "${hanare}/bin/hanare guest unrestrict-userns";
      };
    };
    systemd.services.hanare-audit = lib.mkIf cfg.audit {
      wantedBy = [ "multi-user.target" ];
      before = [ "systemd-user-sessions.service" ];
      serviceConfig = {
        ExecStart = "${hanare}/bin/hanare guest audit -uid 1000";
        Restart = "always";
        RestartSec = "2s";
      };
    };

    # /nix persists across boots, so tampering would outlive a session; check
    # the store's contents against its database shortly after each boot
    # ("hanare status" reports the result).
    systemd.services.hanare-verify-store = {
      serviceConfig = {
        Type = "oneshot";
        Nice = 19;
        IOSchedulingClass = "idle";
        ExecStart = "${config.nix.package}/bin/nix-store --verify --check-contents";
      };
    };
    systemd.timers.hanare-verify-store = {
      wantedBy = [ "timers.target" ];
      timerConfig.OnBootSec = "1min";
    };

    # "hanare switch": rebuild from the host's config, shared read-only at
    # /run/hanare-config, without replacing the disk.
    systemd.services.hanare-switch = {
      serviceConfig.Type = "oneshot";
      path = [ config.nix.package pkgs.git ];
      script = ''
        ${config.system.build.nixos-rebuild}/bin/nixos-rebuild switch --flake path:/run/hanare-config#hanare
      '';
    };

    security.polkit.enable = true;
    security.polkit.extraConfig = ''
      polkit.addRule(function(action, subject) {
        if (action.id != "org.freedesktop.systemd1.manage-units" || subject.user != "${user}")
          return polkit.Result.NOT_HANDLED;
        var unit = action.lookup("unit") || "", verb = action.lookup("verb");
        if (unit == "hanare-switch.service" && verb == "start")
          return polkit.Result.YES;
        if (/^hanare-mount@[^\/]+\.service$/.test(unit) && (verb == "start" || verb == "stop"))
          return polkit.Result.YES;
        return polkit.Result.NOT_HANDLED;
      });
    '';

    nix.settings.experimental-features = [ "nix-command" "flakes" ];

    environment.systemPackages = with pkgs; [ git ];
  };
}
