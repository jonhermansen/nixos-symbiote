{ pkgs, ... }: {
  services.postgresql = {
    enable = true;
    ensureDatabases = [ "runner" ];
    ensureUsers = [{
      name = "runner";
      ensureDBOwnership = true;
    }];
  };

  services.apache-kafka = {
    enable = true;
    settings = {
      "listeners" = [ "PLAINTEXT://localhost:9092" ];
      "log.dirs" = [ "/var/lib/kafka/logs" ];
    };
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

  environment.systemPackages = with pkgs; [
    curl
    jq
    apacheKafka
  ];
}
