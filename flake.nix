{
  description = "Got Nix? Got systemd? Run one command. You're on NixOS.";

  inputs.nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";

  # Project binary cache on Cloudflare R2 (bucket: nixos-symbiote).
  # Local nix users get it automatically after accepting the flake config
  # prompt; CI passes --accept-flake-config to use it non-interactively.
  # NOTE: nix's s3:// `endpoint` param wants a BARE hostname, not a URL — a
  # "https://" prefix makes nix form a malformed request path and R2 returns
  # "Path is invalid (2054)", so nix disables the cache. (The aws CLI's
  # --endpoint-url is the opposite: it DOES want https://.) Keep this bare.
  nixConfig = {
    extra-substituters = [
      "s3://nixos-symbiote?endpoint=e07b6927af7f0bcd485d715838df45ba.r2.cloudflarestorage.com&region=auto"
    ];
    extra-trusted-public-keys = [
      "nixos-symbiote-2:04hzKpsfZ75z6MW8kpALD2gfLuCdnP5fbGdyjHNbsCM="
    ];
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" "x86_64-darwin" ];
      forEachSystem = nixpkgs.lib.genAttrs systems;

      crossSystems = {
        # Use pkgsStatic variants so the Go binary is fully static — no
        # dependency on nix-store glibc or any host libc. Required because
        # the symbiote runs on hosts (Ubuntu CI runners) that have no /nix.
        linux-amd64   = "gnu64";
        linux-arm64   = "aarch64-multiplatform";
        linux-riscv64 = "riscv64";
      };

      # Single source of truth: VERSION file at repo root. flake, Go binary,
      # and git tag all derive from this. Bump VERSION then `git tag v$(cat VERSION)`.
      version = nixpkgs.lib.strings.removeSuffix "\n" (builtins.readFile ./VERSION);

      mkSymbiote = pkgs: pkgs.buildGoModule {
        pname = "nixos-symbiote";
        inherit version;
        src = ./.;
        vendorHash = null;
        env.CGO_ENABLED = "0";
        ldflags = [
          "-s" "-w"
          "-X main.Version=${version}"
        ];
        doCheck = false;
      };

      # VM harness: boots an aarch64 Ubuntu cloud image under QEMU+HVF,
      # cloud-init'd for SSH on port 2222. Wraps test/run-vm with its runtime
      # deps so `nix run .#test-vm` works out of the box on darwin or linux.
      mkTestVm = pkgs: pkgs.writeShellApplication {
        name = "symbiote-test-vm";
        runtimeInputs = with pkgs; [ qemu cdrtools openssh curl coreutils ];
        text = ''
          set -euo pipefail
          SCRIPT_DIR="''${SYMBIOTE_SCRIPT_DIR:-$PWD}"
          WORK="$SCRIPT_DIR/.vm"
          mkdir -p "$WORK"
          if [ ! -f "$WORK/id_ed25519" ]; then
            ssh-keygen -t ed25519 -f "$WORK/id_ed25519" -N "" -q
          fi
          PUBKEY=$(cat "$WORK/id_ed25519.pub")
          IMAGE="$WORK/ubuntu-noble-arm64.img"
          if [ ! -f "$IMAGE" ]; then
            echo "Downloading Ubuntu 24.04 arm64 cloud image..."
            curl -L -o "$IMAGE" \
              "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-arm64.img"
          fi
          DISK="$WORK/disk.qcow2"
          rm -f "$DISK"
          qemu-img create -f qcow2 -b "$IMAGE" -F qcow2 "$DISK" "''${SYMBIOTE_DISK_SIZE:-40G}" 2>/dev/null
          cat > "$WORK/user-data" <<EOF
          #cloud-config
          users:
            - name: ubuntu
              sudo: ALL=(ALL) NOPASSWD:ALL
              shell: /bin/bash
              ssh_authorized_keys:
                - $PUBKEY
          EOF
          echo "instance-id: symbiote-test" > "$WORK/meta-data"
          mkisofs -output "$WORK/seed.iso" -volid cidata -joliet -rock \
            "$WORK/user-data" "$WORK/meta-data" 2>/dev/null
          FW="$(dirname "$(command -v qemu-system-aarch64)")/../share/qemu/edk2-aarch64-code.fd"
          echo "SSH:     ssh -p 2222 -o StrictHostKeyChecking=no -i $WORK/id_ed25519 ubuntu@localhost"
          echo "Monitor: socat STDIO UNIX-CONNECT:$WORK/vm.monitor"
          echo "Serial:  socat STDIO UNIX-CONNECT:$WORK/vm.serial  (Ctrl-O to detach)"
          echo "Booting..."
          # Serial console goes to a unix socket (vm.serial) so we can attach
          # with socat and see kernel + getty output even after SSH dies —
          # critical for debugging post-pivot hangs where sshd isn't up.
          exec qemu-system-aarch64 \
            -M virt -accel hvf -cpu host \
            -m "''${SYMBIOTE_VM_MEM:-4G}" -smp "''${SYMBIOTE_VM_SMP:-4}" \
            -bios "$FW" \
            -drive if=virtio,file="$DISK",format=qcow2 \
            -drive if=virtio,file="$WORK/seed.iso",format=raw \
            -device virtio-net-pci,netdev=net0 \
            -netdev user,id=net0,hostfwd=tcp::2222-:22 \
            -monitor "unix:$WORK/vm.monitor,server,nowait" \
            -serial "unix:$WORK/vm.serial,server,nowait" \
            -display none
        '';
      };

      # Local build check runnable via `nix flake check`.
      mkChecks = pkgs: {
        build = mkSymbiote pkgs;
      };
    in
    {
      packages = forEachSystem (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          # Cross-compiled, FULLY STATIC — targets have no /nix/store or glibc.
          cross = builtins.mapAttrs (_: crossName:
            mkSymbiote pkgs.pkgsCross.${crossName}.pkgsStatic
          ) crossSystems;
          native = mkSymbiote pkgs;
          testVm = mkTestVm pkgs;
        in cross // {
          default = native;
          test-vm = testVm;
        });

      apps = forEachSystem (system: {
        default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/nixos-symbiote";
        };
        test-vm = {
          type = "app";
          program = "${self.packages.${system}.test-vm}/bin/symbiote-test-vm";
        };
      });

      checks = forEachSystem (system:
        mkChecks nixpkgs.legacyPackages.${system});
    };
}
