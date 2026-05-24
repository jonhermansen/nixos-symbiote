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
    boot.isContainer = lib.mkForce true;
    system.stateVersion = lib.mkDefault "25.11";

    fileSystems."/" = lib.mkForce { device = rootDevice; fsType = rootFsType; };
    fileSystems."/nix" = lib.mkIf hasNixMount (lib.mkForce { device = nixDevice; fsType = nixFsType; });

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
    services.resolved.enable = lib.mkDefault false;
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
    # Point the standard ld-linux path at a real linker, not the stub,
    # so host binaries keep working alongside NixOS
    environment.ldso = lib.mkForce "${pkgs.stdenv.cc.bintools.dynamicLinker}";
  })
]).toplevel
