{ configPath, nixpkgsSrc, rootDevice, rootFsType, nixDevice, nixFsType, dnsServers, username, userHome, userUid }:
let
  nixpkgs = fetchTarball {
    url = "https://github.com/NixOS/nixpkgs/archive/${nixpkgsSrc}.tar.gz";
  };
  pkgs = import nixpkgs { system = builtins.currentSystem; };
  lib = pkgs.lib;

  # NB: lib.splitString (not builtins.split) — builtins.split returns the
  # separators as empty LISTS between the pieces, which survive a `s != ""`
  # filter and make nameservers a list-of-lists (type error). splitString
  # yields plain strings; then drop empties from any double/trailing spaces.
  dnsServerList = builtins.filter (s: s != "") (lib.splitString " " dnsServers);
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

    # --- Host the GitHub Actions runner's FOREIGN binaries across the pivot ---
    # The runner (Runner.Listener/.Worker) and its step shells keep running
    # after the soft-reboot, still expecting an Ubuntu userland. NixOS provides
    # none of it by default, so they fail to exec. We bring working, self-
    # contained binaries (nixpkgs builds, patchelf'd to the store loader —
    # NOT an nix-ld shim) and put them where the runner looks.
    #
    # (1) A GNU userland at the FHS paths step shells use. GHA Worker exec's
    #     `bash --noprofile --norc` with an Ubuntu PATH (/usr/bin:/bin); after
    #     the pivot those dirs are NixOS and lack bash/date/sleep/tr/pgrep →
    #     "command not found" (and, with sleep gone, hot-spin loops that burned
    #     15 min + 2.6 GB of logs). These resolve via PATH, so symlinking the
    #     GNU userland into /usr/bin and /bin is enough. /bin/sh is dash — the
    #     SAME shell Ubuntu uses for /bin/sh, so #!/bin/sh scripts behave
    #     identically (busybox ash would subtly differ, GNU is what the runner
    #     assumes everywhere else).
    environment.binsh = "${pkgs.dash}/bin/dash";
    environment.systemPackages = lib.mkBefore [
      pkgs.bash pkgs.dash pkgs.coreutils pkgs.procps pkgs.gnugrep
      pkgs.gnused pkgs.gawk pkgs.findutils pkgs.gnutar pkgs.gzip pkgs.util-linux
      pkgs.git pkgs.systemd pkgs.awscli2
    ];
    system.activationScripts.runnerFhs = let
      userland = pkgs.buildEnv {
        name = "symbiote-fhs-userland";
        paths = with pkgs; [
          bash coreutils procps gnugrep gnused gawk findutils gnutar gzip
          util-linux which
          # Enumerated from the workflow's post-pivot `run:` blocks:
          git        # actions/checkout and its post/cleanup step shell out to it
          systemd    # systemctl / journalctl (Wait-for-NixOS, watchdog, diag)
          awscli2    # `aws s3 cp` — diagnostic uploads to R2
        ];
        ignoreCollisions = true;
      };
    in ''
      mkdir -p /usr/bin /bin
      for f in ${userland}/bin/*; do
        b=$(basename "$f")
        ln -sfn "$f" "/usr/bin/$b"
        ln -sfn "$f" "/bin/$b"
      done
      # sudo is used 36× by the workflow but is SETUID — the store binary isn't
      # (nix store is read-only), so point at NixOS's setuid wrapper instead of
      # the userland buildEnv. security.sudo gives us /run/wrappers/bin/sudo.
      ln -sfn /run/wrappers/bin/sudo /usr/bin/sudo
      ln -sfn /run/wrappers/bin/sudo /bin/sudo
    '';

    # (2) node: the runner executes every JS action (checkout, upload-artifact,
    #     ...) with its OWN bundled node by ABSOLUTE path, so PATH doesn't help.
    #     That node is dynamically linked against glibc at Ubuntu paths and
    #     can't start on NixOS ("Could not start dynamically linked
    #     executable"). Replace it in place with nixpkgs node — same major, but
    #     patchelf'd to the store loader so it runs natively, no nix-ld. Also
    #     drop a node on PATH for steps that call it directly.
    #     The runner's layout varies by image (x86_64:
    #     /home/runner/actions-runner/cached/<ver>/externals/node24/...;
    #     aarch64: /home/runner/extracted/externals/node24/...), so FIND every
    #     bundled node rather than guessing paths.
    system.activationScripts.runnerNode = ''
      for nd in $(${pkgs.findutils}/bin/find /home /opt -maxdepth 8 \
                    -path '*/externals/node*/bin/node' 2>/dev/null); do
        ln -sfn ${pkgs.nodejs_24}/bin/node "$nd"
      done
      ln -sfn ${pkgs.nodejs_24}/bin/node /usr/bin/node
    '';

    # NOTE: do NOT set LogTarget=console with LogLevel=debug here. The QEMU
    # serial socket has no guaranteed reader; a flood of debug writes to
    # /dev/console blocks on a full buffer and deadlocks PID 1 (silent hang).
    # Logs go to the journal (fast); read them post-pivot via the serial
    # getty shell or ssh with `journalctl`.

    # Serial getty on ttyS0 (x86) / ttyAMA0 (aarch64 virt) / hvc0 (riscv virt)
    # with autologin, so we get a live post-pivot shell over the QEMU serial
    # socket even when the network is down. (console= kernelParams are inert
    # on soft-reboot but the host's original cmdline already enables the right
    # serial console on these images.)
    systemd.services."serial-getty@ttyS0".enable = lib.mkDefault true;
    systemd.services."serial-getty@ttyAMA0".enable = lib.mkDefault true;
    systemd.services."serial-getty@hvc0".enable = lib.mkDefault true;
    services.getty.autologinUser = lib.mkDefault username;

    # Bring the interface up via DHCP post-pivot. (An "inherit the live NIC"
    # variant — useDHCP=false — left the link down on Azure/GHA with no
    # connectivity, so DHCP is the proven path; it re-leases the same address,
    # it's just not instant.) nameservers are seeded from the host's REAL
    # upstream resolvers (see dnsServers() in main.go — it skips the
    # systemd-resolved 127.0.0.53 stub) so DNS works immediately regardless of
    # where the host is provisioned.
    networking.useDHCP = lib.mkDefault true;
    networking.nameservers = lib.mkDefault dnsServerList;

    # sshd on the pivoted system so we can SSH straight in to inspect the
    # running runner (far more reliable than the QEMU serial socket). The host
    # user's existing ~/.ssh/authorized_keys rides across the pivot (/home is
    # bind-mounted, mutableUsers keeps it), so the same key that reached Ubuntu
    # reaches NixOS.
    services.openssh.enable = lib.mkDefault true;

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
