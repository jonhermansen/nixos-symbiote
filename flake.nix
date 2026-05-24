{
  description = "Got Nix? Got systemd? Run one command. You're on NixOS.";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forEachSystem = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forEachSystem (system:
        let pkgs = nixpkgs.legacyPackages.${system}; in {
          default = pkgs.writeShellApplication {
            name = "nixos-symbiote";
            runtimeInputs = with pkgs; [ util-linux systemd nix ];
            text = builtins.readFile ./activate.sh;
          };
        });

      apps = forEachSystem (system: {
        default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/nixos-symbiote";
        };
      });
    };
}
