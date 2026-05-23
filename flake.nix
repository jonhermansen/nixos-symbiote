{
  description = "Got Nix? Got systemd? Run one command. You're on NixOS.";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
    flake-parts = {
      url = "github:hercules-ci/flake-parts";
      inputs.nixpkgs-lib.follows = "nixpkgs";
    };
    actions-nix = {
      url = "github:nialov/actions.nix";
      inputs.nixpkgs.follows = "nixpkgs";
      inputs.flake-parts.follows = "flake-parts";
    };
    git-hooks = {
      url = "github:cachix/git-hooks.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    inputs:
    inputs.flake-parts.lib.mkFlake { inherit inputs; } {
      systems = inputs.nixpkgs.lib.systems.flakeExposed;

      imports = [
        inputs.actions-nix.flakeModules.default
        inputs.git-hooks.flakeModule
      ];

      flake.actions-nix = {
        pre-commit.enable = true;
        defaultValues.jobs.runs-on = "ubuntu-24.04";
        workflows = {
          ".github/workflows/test.yaml" = {
            name = "Test NixOS Symbiote";
            on = {
              push.branches = [ "master" ];
              pull_request = { };
              workflow_dispatch = { };
            };
            jobs.smoke-test.steps = [
              { uses = "actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd"; } # v6.0.2
              {
                name = "Migrate to NixOS";
                uses = "./";
                "with".configuration = "example/configuration.nix";
              }
              {
                name = "Verify NixOS is active";
                run = ''
                  echo "Current system: $(readlink /run/current-system)"
                  which psql && which curl && which nginx
                '';
              }
              {
                name = "Run smoke tests";
                run = "./example/smoke-test.sh";
              }
            ];
          };
        };
      };

      perSystem =
        { pkgs, lib, ... }:
        let
          activate = pkgs.writeShellApplication {
            name = "nixos-symbiote";
            runtimeInputs = with pkgs; [
              util-linux
              systemd
              nix
            ];
            text = builtins.readFile ./activate.sh;
          };

          actionDef = {
            name = "NixOS Symbiote";
            description = "Activate NixOS on a running systemd system. No reboot.";
            inputs = {
              configuration = {
                description = "Path to your NixOS configuration.nix";
                required = true;
              };
              nixpkgs = {
                description = "Nixpkgs revision — defaults to flake.lock pin if empty";
                required = false;
                default = "";
              };
              cachix-name = {
                description = "Cachix cache name (enables binary cache)";
                required = false;
                default = "";
              };
            };
            runs = {
              using = "composite";
              steps = [
                {
                  name = "Install Nix";
                  uses = "cachix/install-nix-action@8aa03977d8d733052d78f4e008a241fd1dbf36b3"; # v31.10.6
                }
                {
                  name = "Setup Cachix";
                  "if" = "inputs.cachix-name != ''";
                  uses = "cachix/cachix-action@5f2d7c5294214f71b873db4b969586b980625e71"; # v17
                  "with".name = "\${{ inputs.cachix-name }}";
                }
                {
                  name = "Activate NixOS";
                  shell = "bash";
                  run = ''
                    ${"$"}{{ github.action_path }}/activate.sh \
                      "${"$"}{{ inputs.configuration }}" \
                      "${"$"}{{ inputs.nixpkgs }}"
                  '';
                }
              ];
            };
          };

          pythonEnv = pkgs.python3.withPackages (p: [ p.pyyaml ]);
          actionJson = pkgs.writeTextFile {
            name = "action.json";
            text = builtins.toJSON actionDef;
          };
        in
        {
          packages.default = activate;

          apps.default = {
            type = "app";
            program = lib.getExe activate;
          };

          packages.render-action = pkgs.writeShellApplication {
            name = "render-action";
            runtimeInputs = [ pkgs.git ];
            text = ''
              ROOT=$(git rev-parse --show-toplevel)
              ${pythonEnv}/bin/python3 -c "
              import json, yaml, sys

              def multiline(dumper, data):
                  lines = [l.rstrip() for l in data.splitlines()]
                  fixed = '\n'.join(lines)
                  if len(lines) > 1:
                      return dumper.represent_scalar('tag:yaml.org,2002:str', fixed, style='|')
                  return dumper.represent_scalar('tag:yaml.org,2002:str', fixed)

              yaml.add_representer(str, multiline)

              with open('${actionJson}') as f:
                  action = json.load(f)
              header = '# This file was autogenerated from flake.nix. Do not edit it manually.\n'
              header += '# To regenerate: nix run .#render-action\n'
              sys.stdout.write(header + yaml.dump(action, default_flow_style=False, sort_keys=False))
              " > "$ROOT/action.yml"
              echo "Wrote action.yml"
            '';
          };
        };
    };
}
