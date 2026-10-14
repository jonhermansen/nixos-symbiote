# Acceptance test: a real workload must come up on the pivoted NixOS.
# k3s exercises systemd services, containerd, networking and cgroups — if the
# single-node cluster reaches Ready, the soft-reboot produced a genuinely
# functional NixOS host, not just a booting one.
{ pkgs, ... }: {
  services.k3s = {
    enable = true;
    role = "server";
    # Single-node: no flannel external deps, keep the closure/runtime minimal.
    extraFlags = "--disable traefik --disable metrics-server";
  };
  environment.systemPackages = [ pkgs.k3s ];
  networking.firewall.enable = false;
}
