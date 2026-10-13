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

    # FHS environment for the surviving GitHub Actions runner. The runner is a
    # dynamically-linked .NET app + bundled node, compiled on Ubuntu. After the
    # pivot it must keep running on NixOS, where binaries normally live only in
    # /nix/store. Two NixOS mechanisms make unpatched FHS binaries work:
    #   nix-ld  — installs a stock ld-linux at the conventional interpreter
    #             path (/lib64/ld-linux-x86-64.so.2 etc.) that resolves the
    #             libraries below for any freshly-exec'd child (the Worker
    #             spawns one per step).
    #   envfs   — serves /usr/bin and /bin as a FUSE map onto the system PATH,
    #             so '#!/usr/bin/env' and absolute /usr/bin/<tool> execs resolve.
    programs.nix-ld.enable = lib.mkDefault true;
    programs.nix-ld.libraries = with pkgs; [
      stdenv.cc.cc.lib  # libstdc++, libgcc_s
      zlib
      openssl
      icu
      krb5
      curl
      lttng-ust
      numactl
    ];
    services.envfs.enable = lib.mkDefault true;

    networking.useDHCP = lib.mkDefault true;
    networking.nameservers = lib.mkDefault dnsServerList;

    # Serial console on ttyS0 (x86) + ttyAMA0 (aarch64 virt) + hvc0 (riscv virt).
    # When the activator hangs mid-pivot we attach to the serial socket on the
    # host and see raw kernel + systemd + getty output — no SSH, no network, no
    # D-Bus required. Autologin keeps us from needing a password.
    boot.kernelParams = [ "console=ttyS0,115200" "console=ttyAMA0,115200" "console=hvc0" ];
    systemd.services."serial-getty@ttyS0".enable = lib.mkDefault true;
    systemd.services."serial-getty@ttyAMA0".enable = lib.mkDefault true;
    systemd.services."serial-getty@hvc0".enable = lib.mkDefault true;
    services.getty.autologinUser = lib.mkDefault username;

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
