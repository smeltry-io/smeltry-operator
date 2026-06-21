{
  description = "smeltry-operator — Kubernetes operators for Smeltry";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages.default = pkgs.buildGoModule {
          pname = "smeltry-operator";
          version = self.shortRev or "dev";
          src = ./.;
          vendorHash = null; # set after first `nix build` with `go mod vendor`
          CGO_ENABLED = "0";
          ldflags = [
            "-s" "-w"
            "-extldflags=-static"
          ];
          doCheck = false; # controller-runtime tests require a live cluster
        };

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go_1_22
            golangci-lint
            controller-gen
            kubectl
            kubernetes-helm
          ];
        };
      });
}
