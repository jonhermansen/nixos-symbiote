{ rootDevice, rootFsType, nixDevice, nixFsType, dnsServers, username, userHome, userUid }:
{ lib, ... }:
let
  dnsServerList = builtins.filter (s: s != "") (builtins.split " " dnsServers);
  hasNixMount = nixDevice != "" && nixFsType != "";
  uid = builtins.fromJSON userUid;
in
{
  boot.loader.grub.enable = lib.mkForce false;
  boot.isContainer = lib.mkForce true;
  system.stateVersion = lib.mkDefault "25.11";

  fileSystems."/" = lib.mkForce {
    device = rootDevice;
    fsType = rootFsType;
  };
  fileSystems."/nix" = lib.mkIf hasNixMount (lib.mkForce {
    device = nixDevice;
    fsType = nixFsType;
  });

  networking.useDHCP = lib.mkDefault true;
  networking.nameservers = lib.mkDefault dnsServerList;

  users.users.${username} = lib.mkDefault {
    isNormalUser = true;
    home = userHome;
    uid = uid;
    extraGroups = [ "wheel" ];
  };

  security.sudo.wheelNeedsPassword = lib.mkForce false;

  # Minimize closure — we're a headless CI system, not a workstation
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

}
