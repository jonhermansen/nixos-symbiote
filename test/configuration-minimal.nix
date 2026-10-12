# Minimal config for the CI smoke test. Validates the pivot works end-to-end
# with the smallest possible closure. The full example/configuration.nix with
# k3s + postgres + kafka is a stricter test but blows past the step timeout
# on cold GHA runners.
{ pkgs, ... }: {
  environment.systemPackages = [ pkgs.hello ];
}
