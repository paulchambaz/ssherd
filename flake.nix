{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };
  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};

        buildPkgs = with pkgs; [
          pkg-config
          templ
          scdoc
          go
          tailwindcss_4
        ];

        devPkgs = with pkgs; [
          just
          air
          hivemind
          watchman
        ];

        ssherd = pkgs.buildGoModule {
          pname = "ssherd";
          version = "0.1.0";
          src = ./.;
          vendorHash = "sha256-3tqKrCOhQXAWUBMip+UxBDW9EAiAEXMSqOtfg8qmKT8=";

          nativeBuildInputs = buildPkgs;

          postPatch = ''
            tailwindcss -i static/css/main.css -o static/css/styles.css -m
            templ generate
          '';

          buildPhase = ''
            go test ./tests/...
            mkdir -p bin
            go build -o bin/ssherd ./ssherd
            scdoc < ssherd.1.scd | sed "s/1980-01-01/$(date '+%B %Y')/" > ssherd.1
          '';

          installPhase = ''
            mkdir -p $out/dist/{usr/bin,usr/share/man/man1,etc/ssherd}
            cp bin/ssherd $out/dist/usr/bin/
            cp ssherd.1 $out/dist/usr/share/man/man1/
          '';
        };
      in
      {
        packages = {
          default = ssherd;
          docker = pkgs.dockerTools.buildImage {
            name = "ssherd";
            tag = "latest";

            extraCommands = ''
              mkdir -p var/log/ssherd etc/ssherd
              chmod 755 var/log/ssherd
              touch etc/ssherd/ssherd.cfg
            '';

            config = {
              Cmd = [ "${ssherd}/dist/usr/bin/ssherd" ];
              ExposedPorts = {
                "1265/tcp" = { };
              };
              Env = [
                "SSHERD_SERVER_HOST=0.0.0.0"
              ];
            };
          };
        };
        devShell = pkgs.mkShell {
          nativeBuildInputs = buildPkgs;
          buildInputs = devPkgs;
        };
      }
    );
}
