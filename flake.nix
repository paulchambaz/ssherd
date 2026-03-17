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

        dockerPkgs = with pkgs; [
          nix
          busybox
          git
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
            copyToRoot = pkgs.buildEnv {
              name = "ssherd-env";
              paths = dockerPkgs;
              pathsToLink = [
                "/bin"
                "/usr/bin"
                "/usr/lib"
                "/usr/share"
                "/etc"
              ];
            };
            extraCommands = ''
              mkdir -p var/log/ssherd
              chmod 755 var/log/ssherd
            '';
            config = {
              Cmd = [ "${ssherd}/dist/usr/bin/ssherd" ];
              ExposedPorts = {
                "1321/tcp" = { };
              };
              Env = [ "SSHERD_SERVER_HOST=0.0.0.0" ];
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
