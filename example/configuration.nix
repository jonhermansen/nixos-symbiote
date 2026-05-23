{ pkgs, ... }: {
  services.postgresql = {
    enable = true;
    ensureDatabases = [ "runner" ];
    ensureUsers = [{
      name = "runner";
      ensureDBOwnership = true;
    }];
  };

  services.nginx = {
    enable = true;
    virtualHosts."localhost" = {
      locations."/" = {
        return = "200 'hello from nixos'";
        extraConfig = "default_type text/plain;";
      };
      locations."/health" = {
        return = "200 'ok'";
        extraConfig = "default_type text/plain;";
      };
    };
  };

  virtualisation.docker.enable = true;

  services.k3s = {
    enable = true;
    package = pkgs.k3s_1_36;
    role = "server";
    extraFlags = [ "--snapshotter=nix" ];
  };

  environment.systemPackages = with pkgs; [
    curl
    jq
    kubectl
  ];
}
