{ configPath, nixpkgsSrc, rootDevice, rootFsType, nixDevice, nixFsType, dnsServers, username, userHome, userUid }:
let
  nixpkgs = fetchTarball {
    url = "https://github.com/NixOS/nixpkgs/archive/${nixpkgsSrc}.tar.gz";
  };
  pkgs = import nixpkgs { system = builtins.currentSystem; };
  lib = pkgs.lib;

  dnsServerList = builtins.filter (s: s != "") (builtins.split " " dnsServers);
  hasNixMount = nixDevice != "" && nixFsType != "";
  uid = builtins.fromJSON userUid;
in
(pkgs.nixos [
  (import configPath)
  ({ ... }: {
    boot.loader.grub.enable = lib.mkForce false;
    # NOT a container — soft-reboot keeps the host kernel but we have full
    # kernel capabilities, PIDFDs, cgroup v2, etc. Container mode disables
    # bits of systemd that break "Failed to spawn executor" post-pivot.
    boot.isContainer = lib.mkForce false;
    system.stateVersion = lib.mkDefault "26.05";

    fileSystems."/" = lib.mkForce { device = rootDevice; fsType = rootFsType; };
    # /nix is either a separate mount on the host (hasNixMount) OR it's a
    # subdirectory of the host rootfs. In the latter case we tell new systemd
    # to mount the root device at /nix with x-mount.subdir=nix (systemd 255+)
    # — this means NixOS's own systemd materializes /nix after pivot_root
    # instead of relying on a bind mount from the dying old root.
    fileSystems."/nix" = lib.mkForce (
      if hasNixMount then
        { device = nixDevice; fsType = nixFsType; }
      else
        { device = rootDevice; fsType = rootFsType; options = [ "x-mount.subdir=nix" ]; }
    );

    # Tiny /bin/sh — busybox ash. Keeps the symbiote footprint minimal.
    environment.binsh = "${pkgs.busybox}/bin/sh";
    # GitHub Actions Worker exec's /bin/bash --noprofile --norc for every
    # step shell. NixOS doesn't put bash at /bin/bash by default — all
    # binaries live in /nix/store. Without this, Worker's execve returns
    # ENOENT and the whole step hangs. Create fixed-path symlinks via
    # activation.
    environment.systemPackages = lib.mkBefore [ pkgs.busybox pkgs.bash ];
    system.activationScripts.binBash = ''
      ln -sfn ${pkgs.bash}/bin/bash /bin/bash
      mkdir -p /usr/bin
      ln -sfn ${pkgs.bash}/bin/bash /usr/bin/bash
    '';

    networking.useDHCP = lib.mkDefault true;
    networking.nameservers = lib.mkDefault dnsServerList;

    users.users.${username} = lib.mkDefault {
      isNormalUser = true;
      home = userHome;
      inherit uid;
      extraGroups = [ "wheel" ];
    };
    # Preserve host users/groups — don't remove or remap existing UIDs/GIDs
    users.mutableUsers = lib.mkForce true;
    security.sudo.wheelNeedsPassword = lib.mkForce false;
    security.sudo.extraRules = lib.mkForce [{
      users = [ username ];
      commands = [{ command = "ALL"; options = [ "NOPASSWD" ]; }];
    }];

    networking.firewall.enable = lib.mkDefault false;
    systemd.oomd.enable = lib.mkDefault false;
    documentation.enable = lib.mkDefault false;
    xdg.icons.enable = lib.mkDefault false;
    xdg.sounds.enable = lib.mkDefault false;
    xdg.mime.enable = lib.mkDefault false;
    nix.enable = lib.mkDefault false;
    boot.supportedFilesystems = lib.mkDefault [ ];
    system.disableInstallerTools = lib.mkDefault true;
    programs.command-not-found.enable = lib.mkDefault false;
    environment.defaultPackages = lib.mkDefault [ ];
    fonts.fontconfig.enable = lib.mkDefault false;
  })
]).toplevel
