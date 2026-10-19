# Minimal config for the CI smoke test. Validates the pivot works end-to-end
# with the smallest possible closure. The full example/configuration.nix with
# k3s + postgres + kafka is a stricter test but blows past the step timeout
# on cold GHA runners.
{ pkgs, ... }: {
  # hello: the acceptance smoke test (a NixOS binary runs post-pivot).
  # fastfetch: a visual proof-of-pivot for the end of the run — it prints
  # `OS: NixOS` with `Kernel: <the retained Ubuntu kernel>`, which is the
  # whole trick in one panel (new userland, same kernel).
  environment.systemPackages = [ pkgs.hello pkgs.fastfetch ];
}
