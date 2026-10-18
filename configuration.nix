{ config, lib, modulesPath, pkgs, ... }:

let
  cfg = builtins.fromJSON (builtins.readFile ./hanare.json);
  user = cfg.user;
  home = "/home/${user}";

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
      start) exec ${pkgs.util-linux}/bin/mount -t virtiofs -o "$mode,nosuid,nodev" -- "$tag" "$real" ;;
      stop) exec ${pkgs.util-linux}/bin/umount -- "$real" ;;
    esac
  '';
in
{
  imports = [ "${modulesPath}/profiles/qemu-guest.nix" ];

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
  # (the image is truncated to "disk" in hanare.json by "hanare reinstall").
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

  # passt (user-mode networking): plain DHCP on the virtio NIC.
  networking.useNetworkd = true;
  networking.useDHCP = true;
  networking.nftables.enable = true;
  networking.firewall.enable = true;
  # "lan": false in hanare.json: no traffic to private/LAN, Tailscale (CGNAT)
  # or link-local addresses; DNS goes to passt's forwarder (192.0.2.53).
  networking.nftables.tables.hanare-egress = lib.mkIf (!cfg.lan) {
    family = "inet";
    content = ''
      chain output {
        type filter hook output priority filter; policy accept;
        oifname "lo" accept
        ip daddr 192.0.2.53 accept
        udp dport 67 accept
        ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10, 169.254.0.0/16 } reject
        meta l4proto ipv6-icmp accept
        ip6 daddr { fc00::/7, fe80::/10 } reject
      }
    '';
  };

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
    hashedPassword = "!";
    # Public half of ~/.local/state/hanare/ssh/id_ed25519.
    openssh.authorizedKeys.keys = [ cfg.ssh_authorized_key ];
  };
  users.groups.${user}.gid = 1000;
  security.sudo.enable = false;
  users.allowNoPasswordLogin = true;

  # SSH only over vsock (port 22, socket-activated per connection); no TCP listener.
  services.openssh = {
    enable = true;
    openFirewall = false;
    listenAddresses = [ { addr = "127.0.0.1"; port = 22; } ];
    # Persisted so the host's pinned key stays valid across reboots.
    hostKeys = [ { path = "/persist/ssh/ssh_host_ed25519_key"; type = "ed25519"; } ];
    settings = {
      PermitRootLogin = "no";
      PasswordAuthentication = false;
      KbdInteractiveAuthentication = false;
    };
  };
  systemd.services.sshd.enable = false;
  systemd.sockets.hanare-ssh = {
    wantedBy = [ "sockets.target" ];
    socketConfig = {
      ListenStream = "vsock::22";
      Accept = true;
    };
  };
  systemd.services."hanare-ssh@" = {
    after = [ "sshd-keygen.service" ];
    wants = [ "sshd-keygen.service" ];
    serviceConfig = {
      ExecStart = "-${config.services.openssh.package}/bin/sshd -i -f /etc/ssh/sshd_config";
      StandardInput = "socket";
      StandardError = "journal";
    };
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

  # Tools available in the VM; apply changes with "hanare switch".
  environment.systemPackages = with pkgs; [ git vim tree ];
}
