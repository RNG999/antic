# Guest side of hanare: a stateless NixOS guest that boots directly from a
# kernel and initrd the host built, with / on tmpfs and /nix read-only from
# the host (virtiofs). Import this module (nixosModules.guest) from your guest
# flake and set hanare.user; see README.md.
{ config, lib, pkgs, ... }:

let
  cfg = config.hanare;
  proto = lib.importJSON ./proto.json;
  user = cfg.user;
  account = config.users.users.${user};

  # sshd for one vsock connection (see the banken-session sockets). Without
  # PAM, the session's processes stay in the per-connection unit's cgroup,
  # so the restrictions below apply to everything the session runs.
  sessionService = {
    requires = [ "banken-ready.target" ];
    after = [ "banken-ready.target" ];
    # "hanare guest apply" must not restart sessions, including its own.
    restartIfChanged = false;
    stopIfChanged = false;
    environment.LD_LIBRARY_PATH = config.system.nssModules.path;
    serviceConfig = {
      ExecStart = "-${config.services.openssh.package}/bin/sshd -i -f /etc/ssh/sshd_config";
      StandardInput = "socket";
      StandardError = "journal";
      NoNewPrivileges = true;
      RestrictSUIDSGID = true;
      RestrictNamespaces = true;
      LockPersonality = true;
      ProtectKernelTunables = true;
      ProtectKernelModules = true;
      ProtectKernelLogs = true;
      ProtectControlGroups = true;
      ProtectClock = true;
      ProtectHostname = true;
      PrivateDevices = true;
      SystemCallArchitectures = "native";
      # No "~@mount": sshd's privilege separation chroot()s. Mounting needs
      # CAP_SYS_ADMIN, which the bounding set below leaves out.
      SystemCallFilter = [ "~@module" "~@reboot" "~@swap" "~@raw-io" "~@obsolete" "~@clock" "~@cpu-emulation" ];
      CapabilityBoundingSet = [ "CAP_SETUID" "CAP_SETGID" "CAP_CHOWN" "CAP_DAC_OVERRIDE" "CAP_SYS_CHROOT" "CAP_KILL" "CAP_AUDIT_WRITE" ];
    };
  };
in
{
  options.hanare = {
    user = lib.mkOption {
      type = lib.types.passwdEntry lib.types.str;
      description = "Guest login user; must be the host user that runs hanare (UID/GID 1000 in the guest).";
    };
    package = lib.mkOption {
      type = lib.types.package;
      default = (import ./packages.nix { inherit pkgs; }).banken;
      defaultText = lib.literalMD "banken from this flake";
      description = "The banken package (guest agent and BPF programs).";
    };
    restrictUserNamespaces = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Deny creating user namespaces to everyone but root (BPF LSM), a common
        first step of kernel exploits. Sessions additionally run with
        RestrictNamespaces=.
      '';
    };
    audit = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = ''
        Log the guest user's executions, connections and files opened for
        writing (BPF) to a serial port the host appends to a file; see
        "hanare audit".
      '';
    };
  };

  config = {
    assertions = [
      { assertion = account.uid == 1000; message = "hanare: users.users.${user}.uid must be 1000 (virtiofsd maps only that UID)."; }
      { assertion = user != "root"; message = "hanare: hanare.user must not be root."; }
      {
        assertion = !(cfg.audit || cfg.restrictUserNamespaces) || lib.elem "bpf" config.security.lsm;
        message = "hanare: audit and restrictUserNamespaces need \"bpf\" in security.lsm.";
      }
    ];

    system.stateVersion = lib.mkDefault "26.05";
    networking.hostName = lib.mkDefault "hanare";

    # Booted directly by the host (kernel, initrd, init=); no boot loader.
    boot.loader.grub.enable = false;
    boot.initrd.systemd.enable = true;
    boot.initrd.includeDefaultModules = false;
    boot.initrd.availableKernelModules = [ "virtio_pci" "virtio_console" "virtiofs" ];
    # dmi_sysfs: lets systemd read the host's credentials from SMBIOS.
    boot.initrd.kernelModules = [ "dmi_sysfs" ];
    # Loaded at boot: module loading is locked afterwards.
    boot.kernelModules = [ "vmw_vsock_virtio_transport" "virtio_net" "virtiofs" ];
    boot.kernelParams = [
      "console=hvc0"
      "systemd.ssh_auto=0"
      "lockdown=integrity"
      "slab_nomerge"
      "init_on_alloc=1"
      "init_on_free=1"
      "page_alloc.shuffle=1"
      "randomize_kstack_offset=on"
      "vsyscall=none"
      "debugfs=off"
    ];
    security.lsm = lib.mkAfter [ "lockdown" ];
    security.lockKernelModules = true;
    security.protectKernelImage = true;
    boot.kernel.sysctl = {
      "kernel.io_uring_disabled" = 2;
      "kernel.unprivileged_bpf_disabled" = 1;
      "kernel.perf_event_paranoid" = 2;
      "kernel.kptr_restrict" = 2;
      "kernel.dmesg_restrict" = 1;
      "kernel.kexec_load_disabled" = 1;
      "kernel.sysrq" = 0;
      "vm.unprivileged_userfaultfd" = 0;
      "dev.tty.ldisc_autoload" = 0;
      "net.core.bpf_jit_harden" = 2;
    };

    # Nothing persists in the guest: / is tmpfs, /nix is the host's.
    fileSystems."/" = {
      device = "none";
      fsType = "tmpfs";
      options = [ "mode=755" "size=50%" ];
    };
    fileSystems."/nix" = {
      device = proto.storeTag;
      fsType = "virtiofs";
      options = [ "ro" "nodev" ];
      neededForBoot = true;
    };
    nix.enable = false;
    system.etc.overlay.enable = true;
    services.userborn.enable = true;
    services.journald.storage = "volatile";
    documentation.enable = false;
    environment.defaultPackages = [ ];
    systemd.services."serial-getty@hvc0".enable = false;
    systemd.services."getty@tty1".enable = false;
    security.sudo.enable = false;
    security.polkit.enable = false;
    security.wrappers = lib.mkForce { };

    # passt (vhost-user networking): plain DHCP on the virtio NIC. Egress is
    # filtered on the host, where guest root cannot change it.
    networking.useNetworkd = true;
    networking.useDHCP = true;
    systemd.network.wait-online.enable = false;
    networking.nftables.enable = true;
    networking.firewall.enable = true;

    users.mutableUsers = false;
    users.allowNoPasswordLogin = true;
    users.users.root.hashedPassword = "!";
    users.users.${user} = {
      isNormalUser = true;
      uid = 1000;
      # Primary GID must match the host (1000): virtiofsd maps only 1000:1000.
      group = user;
      # "*" rather than "!": without PAM, OpenSSH refuses "!"-locked accounts.
      hashedPassword = "*";
    };
    users.groups.${user}.gid = 1000;

    # SSH over vsock only, with the keys the host passes as credentials.
    services.openssh = {
      enable = true;
      openFirewall = false;
      startWhenNeeded = false;
      hostKeys = [ ];
      authorizedKeysInHomedir = false;
      authorizedKeysFiles = lib.mkForce [ "${proto.runDir}/authorized_keys" ];
      extraConfig = "HostKey ${proto.runDir}/ssh_host_ed25519_key";
      settings = {
        UsePAM = false;
        PermitRootLogin = "no";
        PasswordAuthentication = false;
        KbdInteractiveAuthentication = false;
        AllowUsers = [ user ];
        DisableForwarding = true;
        PermitTunnel = "no";
        PermitUserRC = false;
        PermitUserEnvironment = false;
        MaxAuthTries = 2;
      };
    };
    systemd.services.sshd.enable = false;
    systemd.services.sshd-keygen.enable = false;

    systemd.services.banken-ssh-keys = {
      requiredBy = [ "banken-ready.target" ];
      before = [ "banken-ready.target" ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ImportCredential = [ proto.credAuthorizedKeys proto.credHostKey ];
        RuntimeDirectory = "hanare";
        RuntimeDirectoryPreserve = true;
      };
      # "restrict,pty": no forwarding of any kind, only a terminal.
      script = ''
        ${pkgs.gnused}/bin/sed 's/^/restrict,pty /' "$CREDENTIALS_DIRECTORY/${proto.credAuthorizedKeys}" > ${proto.runDir}/authorized_keys
        chmod 0644 ${proto.runDir}/authorized_keys
        install -m 0600 "$CREDENTIALS_DIRECTORY/${proto.credHostKey}" ${proto.runDir}/ssh_host_ed25519_key
      '';
    };

    # Port 22: normal sessions; port 23: offline sessions.
    systemd.sockets.banken-session = {
      wantedBy = [ "sockets.target" ];
      socketConfig = {
        ListenStream = "vsock::${toString proto.portSession}";
        Accept = true;
      };
    };
    systemd.sockets.banken-session-offline = {
      wantedBy = [ "sockets.target" ];
      socketConfig = {
        ListenStream = "vsock::${toString proto.portSessionOffline}";
        Accept = true;
      };
    };
    systemd.services."banken-session@" = sessionService;
    systemd.services."banken-session-offline@" = lib.recursiveUpdate sessionService {
      # No network and no way around it: no IP, only AF_UNIX, and neither the
      # resolver (also via nsncd), D-Bus nor vsock; /tmp and IPC are private.
      serviceConfig = {
        PrivateNetwork = true;
        IPAddressDeny = "any";
        RestrictAddressFamilies = [ "AF_UNIX" ];
        PrivateTmp = true;
        PrivateIPC = true;
        InaccessiblePaths = [ "-/run/nscd" "-/run/systemd/resolve" "-/run/dbus" "-/run/systemd/private" ];
      };
    };

    # Everything a session relies on: keys, configured shares (fstab.extra),
    # the user namespace restriction and the audit log. Sessions require it,
    # so they never start without these.
    systemd.targets.banken-ready = {
      wantedBy = [ "multi-user.target" ];
      after = [ "local-fs.target" "remote-fs.target" ];
    };

    systemd.services.banken-agent = {
      description = "banken agent (serves the host on vsock port ${toString proto.portAgent})";
      wantedBy = [ "multi-user.target" ];
      restartIfChanged = false;
      path = [ config.systemd.package ];
      serviceConfig = {
        Type = "notify";
        ExecStart = "${cfg.package}/bin/banken agent --user ${user}";
        Restart = "always";
        RestartSec = "1s";
      };
    };

    systemd.services.banken-restrict-userns = lib.mkIf cfg.restrictUserNamespaces {
      requiredBy = [ "banken-ready.target" ];
      before = [ "banken-ready.target" ];
      after = [ "sys-fs-bpf.mount" ];
      # Restarting would only re-pin; never leave a gap during "apply".
      restartIfChanged = false;
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${cfg.package}/bin/banken restrict-userns";
      };
    };

    systemd.services.banken-audit = lib.mkIf cfg.audit {
      requiredBy = [ "banken-ready.target" ];
      before = [ "banken-ready.target" ];
      restartIfChanged = false;
      unitConfig.StartLimitIntervalSec = 0;
      serviceConfig = {
        Type = "notify";
        ExecStart = "${cfg.package}/bin/banken audit --uid ${toString account.uid} --out ${proto.auditPort}";
        Restart = "always";
        RestartSec = "1s";
        OOMScoreAdjust = -1000;
      };
    };

    environment.systemPackages = [ ];
  };
}
