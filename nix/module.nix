# Guest side of antic: a minimal, Perl-less stateless NixOS microVM that boots
# directly from a kernel and initrd the host built, with / on tmpfs and /nix
# read-only from the host (virtiofs). Import this module (nixosModules.guest)
# from your guest flake and set antic.user; see README.md.
{ config, lib, pkgs, modulesPath, ... }:

let
  cfg = config.antic;
  proto = lib.importJSON ./proto.json;
  user = cfg.user;
  account = config.users.users.${user};

  # One antic-agent session instance per vsock connection (see the
  # antic-session sockets). Processes stay in the per-connection unit's
  # cgroup with zero capabilities and strict systemd sandboxing.
  sessionService = {
    requires = [ "antic-ready.target" ];
    after = [ "antic-ready.target" ];
    # "antic guest apply" must not restart sessions, including its own.
    restartIfChanged = false;
    stopIfChanged = false;
    path = [ "/run/current-system/sw" ];
    environment = {
      HOME = account.home;
      USER = user;
      LOGNAME = user;
      SHELL = "/run/current-system/sw/bin/bash";
    };
    serviceConfig = {
      User = user;
      Group = user;
      ExecStart = "${cfg.package}/bin/antic-agent session";
      StandardInput = "socket";
      StandardOutput = "socket";
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
      SystemCallFilter = [ "~@mount" "~@module" "~@reboot" "~@swap" "~@raw-io" "~@obsolete" "~@clock" "~@cpu-emulation" ];
      CapabilityBoundingSet = [ ];
    };
  };
in
{
  imports = [
    "${modulesPath}/profiles/perlless.nix"
    "${modulesPath}/profiles/minimal.nix"
  ];

  options.antic = {
    user = lib.mkOption {
      type = lib.types.passwdEntry lib.types.str;
      description = "Guest login user; must be the host user that runs antic (UID/GID 1000 in the guest).";
    };
    package = lib.mkOption {
      type = lib.types.package;
      default = (import ./packages.nix { inherit pkgs; }).antic-agent;
      defaultText = lib.literalMD "antic-agent from this flake";
      description = "The antic-agent package (guest agent and session runner).";
    };
  };

  config = {
    assertions = [
      { assertion = account.uid == 1000; message = "antic: users.users.${user}.uid must be 1000 (virtiofsd maps only that UID)."; }
      { assertion = user != "root"; message = "antic: antic.user must not be root."; }
    ];

    system.stateVersion = lib.mkDefault "26.05";
    networking.hostName = lib.mkDefault "antic";

    # Booted directly by the host (kernel, initrd, init=); no boot loader,
    # Rust stage-2 nixos-init, and no hardware/desktop/storage cruft.
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

    # Distroless / microVM minimization: strip unused subsystems and packages.
    nix.enable = false;
    system.nixos-init.enable = true;
    system.etc.overlay.enable = true;
    services.userborn.enable = true;
    services.journald.storage = "volatile";
    services.lvm.enable = false;
    boot.bcache.enable = false;
    boot.kexec.enable = false;
    boot.enableContainers = false;
    programs.fuse.enable = false;
    programs.nano.enable = false;
    fonts.fontconfig.enable = false;
    powerManagement.enable = false;
    console.enable = false;
    documentation.enable = false;
    environment.defaultPackages = [ ];
    systemd.enableEmergencyMode = false;
    systemd.services."serial-getty@ttyS0".enable = false;
    systemd.services."serial-getty@hvc0".enable = false;
    systemd.services."getty@tty1".enable = false;
    systemd.services."autovt@".enable = false;
    security.sudo.enable = false;
    security.polkit.enable = false;
    security.enableWrappers = false;

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
      hashedPassword = "!";
    };
    users.groups.${user}.gid = 1000;

    # Port 22: normal sessions; port 23: offline sessions.
    systemd.sockets.antic-session = {
      wantedBy = [ "sockets.target" ];
      socketConfig = {
        ListenStream = "vsock::${toString proto.portSession}";
        Accept = true;
      };
    };
    systemd.sockets.antic-session-offline = {
      wantedBy = [ "sockets.target" ];
      socketConfig = {
        ListenStream = "vsock::${toString proto.portSessionOffline}";
        Accept = true;
      };
    };
    systemd.services."antic-session@" = sessionService;
    systemd.services."antic-session-offline@" = lib.recursiveUpdate sessionService {
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

    # Everything a session relies on: configured shares (fstab.extra).
    # Sessions require it, so they never start without these.
    systemd.targets.antic-ready = {
      wantedBy = [ "multi-user.target" ];
      after = [ "local-fs.target" "remote-fs.target" ];
    };

    systemd.services.antic-agent = {
      description = "antic-agent (serves the host on vsock port ${toString proto.portAgent})";
      wantedBy = [ "multi-user.target" ];
      restartIfChanged = false;
      path = [ config.systemd.package ];
      serviceConfig = {
        Type = "notify";
        ExecStart = "${cfg.package}/bin/antic-agent agent --user ${user}";
        Restart = "always";
        RestartSec = "1s";
      };
    };

    environment.systemPackages = [ ];
  };
}
