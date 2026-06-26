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
          ldflags = [
            "-s" "-w"
            "-extldflags=-static"
          ];
          doCheck = false; # controller-runtime tests require a live cluster
        };

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go # tracks the current stable Go in nixpkgs (go_1_22 was removed)
            golangci-lint
            # controller-gen is not a top-level nixpkgs package;
            # use `task tools` to download it via go install (see Taskfile.yaml)
            kubectl
            kubernetes-helm
          ];
        };
      });
}
