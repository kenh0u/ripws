{
  description = "RIPWS - Raw IP over WebSocket (server & client)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    {
      # NixOS module:
      #   services.ripws.tunnels.<name> = { ... };
      nixosModules.default = import ./nix/module.nix;
      nixosModules.ripws = self.nixosModules.default;

      # Make pkgs.ripws available: inputs.ripws.overlays.default
      overlays.default = final: prev: {
        ripws = self.packages.${final.system}.ripws or final.callPackage ./nix/package.nix { };
      };
    } //
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        version = "0.1.0";
        ripws = pkgs.callPackage ./nix/package.nix { inherit version; };
        ripwsOpenBSD = targetArch: pkgs.callPackage ./nix/package.nix {
          inherit version targetArch;
          targetOs = "openbsd";
        };
        ripws-openbsd-amd64 = ripwsOpenBSD "amd64";
        ripws-openbsd-arm64 = ripwsOpenBSD "arm64";
      in
      {
        packages = {
          default = ripws;
          inherit ripws ripws-openbsd-amd64 ripws-openbsd-arm64;
        };
        checks = {
          # Pure-eval tests of the NixOS module (fast, no builds).
          ripws-eval = import ./nix/eval-test.nix { inherit pkgs ripws; module = ./nix/module.nix; };
          # Full end-to-end VM test (needs KVM).
          ripws-vm = import ./nix/vm-test.nix { inherit pkgs; ripwsPackage = ripws; };
          # Cross-compile verification for OpenBSD targets.
          inherit ripws-openbsd-amd64 ripws-openbsd-arm64;
        };
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [ go gopls gotools ];
        };
      });
}
