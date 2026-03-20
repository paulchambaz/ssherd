{
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    docker-nixpkgs = {
      url = "github:nix-community/docker-nixpkgs";
      flake = false;
    };
  };
  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      docker-nixpkgs,
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
          vendorHash = "sha256-j+wTXFuMlALs82PdAhZsm6LBtFr4a9MBqJuaFvIN92o=";
          nativeBuildInputs = buildPkgs;
          postPatch = ''
            tailwindcss --input static/css/main.css --output static/css/styles.css --minify --optimize
            templ generate
          '';
          buildPhase = ''
            go test ./tests/...
            mkdir -p bin
            go build -o bin/ssherd .
            scdoc < ssherd.1.scd | sed "s/1980-01-01/$(date '+%B %Y')/" > ssherd.1
          '';
          installPhase = ''
            mkdir -p $out/bin $out/share/man/man1
            cp bin/ssherd $out/bin/ssherd
            cp ssherd.1   $out/share/man/man1/ssherd.1
          '';
        };
        buildImageWithNix = import ("${docker-nixpkgs}" + "/images/nix/default.nix");
        nixBaseImage = buildImageWithNix {
          inherit (pkgs)
            dockerTools
            bashInteractive
            cacert
            coreutils
            curl
            gnutar
            gzip
            iana-etc
            nix
            openssh
            xz
            ;
          gitReallyMinimal = pkgs.git;
          extraContents = [ ];
        };
      in
      {
        packages = {
          default = ssherd;
          docker = pkgs.dockerTools.buildImage {
            name = "ssherd";
            tag = "latest";
            fromImage = nixBaseImage;
            copyToRoot = pkgs.buildEnv {
              name = "ssherd-env";
              paths = [ ssherd ];
              pathsToLink = [ "/" ];
            };
            extraCommands = ''
              mkdir -p var/log/ssherd
              mkdir -p root
            '';
            config = {
              Cmd = [ "/bin/ssherd" ];
              ExposedPorts = {
                "1321/tcp" = { };
              };
              Env = [
                "SSHERD_SERVER_HOST=0.0.0.0"
                "SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
                "GIT_SSL_CAINFO=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt"
                "NIX_CONFIG=experimental-features = nix-command flakes\nsandbox = false\ntrusted-users = root"
                "HOME=/root"
                "USER=root"
                "NIX_REMOTE=daemon"
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
