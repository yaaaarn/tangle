{
  description = "tangle";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    (flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages.default = pkgs.callPackage ./package.nix { };
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gcc
          ];
        };
      }
    ))
    // {
      nixosModules.default = { config, lib, pkgs, ... }: 
        let
          cfg = config.services.tangle;
          yamlConfig = pkgs.writers.writeYAML "config.yaml" {
            actions = cfg.actions;
          };
        in {
          options.services.tangle = {
            enable = lib.mkEnableOption "Tangle daemon";
            package = lib.mkOption {
              type = lib.types.package;
              default = self.packages.${pkgs.system}.default;
              description = "The tangle package to use.";
            };
            actions = lib.mkOption {
              type = lib.types.listOf lib.types.attrs;
              default = [];
              description = "List of action hooks.";
            };
          };

          config = lib.mkIf cfg.enable {
            environment.etc."tangle/config.yaml".source = yamlConfig;

            systemd.services.tangle = {
              description = "tangle daemon";
              wantedBy = [ "multi-user.target" ];
              after = [ "dbus.service" ];
              
              serviceConfig = {
                ExecStart = "${cfg.package}/bin/tangle";
                Restart = "on-failure";
                RestrictAddressFamilies = [ "AF_UNIX" ]; 
              };
            };
          };
        };

      hmModules.default = { config, lib, pkgs, ... }:
        let
          cfg = config.services.tangle;
        in {
          options.services.tangle = {
            enable = lib.mkEnableOption "Tangle daemon";
            package = lib.mkOption {
              type = lib.types.package;
              default = self.packages.${pkgs.system}.default;
              description = "The tangle package to use.";
            };
            actions = lib.mkOption {
              type = lib.types.listOf lib.types.attrs;
              default = [];
              description = "List of action hooks.";
            };
          };

          config = lib.mkIf cfg.enable {
            xdg.configFile."tangle/config.yaml".text = lib.generators.toYAML {} {
              actions = cfg.actions;
            };

            systemd.user.services.tangle = {
              Unit = {
                Description = "tangle daemon (user service)";
                After = [ "graphical-session.target" ];
              };
              Install = {
                WantedBy = [ "graphical-session.target" ];
              };
              Service = {
                ExecStart = "${cfg.package}/bin/tangle";
                Restart = "on-failure";
              };
            };
          };
        };
    };
}
