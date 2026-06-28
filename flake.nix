{
  description = "stitchfs - edit several files as one; saving splits the changes back into the originals";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        version = "0.4.0";
      in {
        packages.default = pkgs.buildGoModule {
          pname = "stitchfs";
          inherit version;
          src = ./.;
          vendorHash = "sha256-RoelsTrvGjQZeFIa8PupdkmBObe9gfoTpggpICAro7Y=";
          ldflags = [ "-s" "-w" "-X" "main.version=${version}" ];
          meta = with pkgs.lib; {
            description = "Edit several files as one virtual file; saving splits the changes back";
            homepage = "https://github.com/eric-frost/stitchfs";
            license = licenses.mit;
            mainProgram = "stitchfs";
            platforms = platforms.linux ++ platforms.darwin;
          };
        };

        apps.default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/stitchfs";
        };
      });
}
