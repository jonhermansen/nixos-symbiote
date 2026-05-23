{ configPath, nixpkgsSrc, rootDevice, rootFsType, nixDevice, nixFsType, dnsServers, username, userHome, userUid }:
let
  nixpkgs = fetchTarball {
    url = "https://github.com/NixOS/nixpkgs/archive/${nixpkgsSrc}.tar.gz";
  };
  pkgs = import nixpkgs { system = builtins.currentSystem; };
in
(pkgs.nixos [
  (import configPath)
  (import ./base-module.nix { inherit rootDevice rootFsType nixDevice nixFsType dnsServers username userHome userUid; })
]).toplevel
