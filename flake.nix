{
  description = "wg6pn: join a Fly.io org's private network from a sprite";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in {
      packages = forAll (pkgs: rec {
        wg6pn = pkgs.buildGoModule {
          pname = "wg6pn";
          version = self.shortRev or "dirty";
          src = ./.;
          subPackages = [ "cmd/wg6pn" ];
          vendorHash = "sha256-cccuI6tsluI4w+o6sPc7R4YKYZw7Bjx2BDeldXMMwfM=";
          env.CGO_ENABLED = "0";
          ldflags = [ "-s" "-w" ];
          __darwinAllowLocalNetworking = true;
          meta.mainProgram = "wg6pn";
        };
        default = wg6pn;
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell { packages = [ pkgs.go pkgs.gopls ]; };
      });
    };
}
