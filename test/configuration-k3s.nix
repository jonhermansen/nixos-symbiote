# Acceptance test: a real workload must come up on the pivoted NixOS.
# k3s exercises systemd services, containerd, networking and cgroups — if the
# single-node cluster reaches Ready, the soft-reboot produced a genuinely
# functional NixOS host, not just a booting one.
{ pkgs, config, ... }: {
  services.k3s = {
    enable = true;
    role = "server";
    # k3s package is conditional on the BUILD platform:
    #   - Linux build (the pivot target + CI): stock upstream pkgs.k3s — the
    #     node has real namespaces, no patches needed.
    #   - Darwin build (the darwin-vm cross-dev loop): the kubernix-patched
    #     k3s that runs without linux namespaces. Those patches live in the
    #     jon/darwin nixpkgs fork (not upstream), so wiring them here means
    #     adding that fork/overlay as a source — a deliberate integration step.
    # Today only the stock (linux) path is wired; the darwin branch is the
    # documented hook for when the darwin-vm dev loop is integrated.
    package =
      if pkgs.stdenv.buildPlatform.isDarwin
      then pkgs.k3s # TODO: swap for kubernix-patched k3s from jon/darwin fork
      else pkgs.k3s;
    # Single-node: no flannel external deps, keep the closure/runtime minimal.
    extraFlags = "--disable traefik --disable metrics-server";
  };
  environment.systemPackages = [ config.services.k3s.package ];
  networking.firewall.enable = false;
}
