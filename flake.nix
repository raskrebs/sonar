{
  description = "Nix flake for the sonar CLI";

  inputs = {
    nixpkgs = {
      url = "https://github.com/NixOS/nixpkgs/archive/b40629efe5d6ec48dd1efba650c797ddbd39ace0.tar.gz";
      flake = false;
    };
  };

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems =
        fn:
        builtins.listToAttrs (
          map (system: {
            name = system;
            value = fn system;
          }) systems
        );
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
          buildGo125Module = pkgs.buildGoModule.override {
            go = pkgs.go_1_25;
          };
          version = "dev";
        in
        {
          default = buildGo125Module {
            pname = "sonar";
            inherit version;
            src = ./.;
            modRoot = "cli";
            subPackages = [ "." ];
            vendorHash = "sha256-komX1AmHt2NoF1x6xsNa2RFkfVzOXfYEMPhT0zwMxjw=";

            ldflags = [
              "-s"
              "-w"
              "-X"
              "github.com/raskrebs/sonar/internal/selfupdate.Version=${version}"
            ];

            meta = with pkgs.lib; {
              description = "Know what's running on your machine.";
              homepage = "https://github.com/raskrebs/sonar";
              license = licenses.mit;
              mainProgram = "sonar";
              platforms = platforms.linux ++ platforms.darwin;
            };
          };
        }
      );

      devShells = forAllSystems (
        system:
        let
          pkgs = import nixpkgs { inherit system; };
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.go_1_25
              pkgs.gopls
            ];
          };
        }
      );
    };
}
