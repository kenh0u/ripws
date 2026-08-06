{
  description = "RIPWS - Raw IP over WebSocket (server & client)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        version = "0.1.0";
        ripws = pkgs.buildGoModule {
          pname = "ripws";
          inherit version;
          src = ./.;
          # `subPackages` defaults to "."; we only have the root package as
          # the binary entry point.
          subPackages = [ "." ];
          # Computed with `nix build` (lib.fakeHash then read the got hash).
          vendorHash = "sha256-2oHinTZ5MU464sDS9faZdGC7ggiKLac8PR9Xod9OcyY=";
          # Pure-Go static build so the binary runs in any minimal container
          # (no nix glibc loader needed).
          env.CGO_ENABLED = 0;
          meta = with pkgs.lib; {
            description = "Raw IP over WebSocket tunnel";
            license = licenses.mit;
            mainProgram = "ripws";
          };
        };
      in
      {
        packages = {
          default = ripws;
          ripws = ripws;
        };
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [ go gopls gotools ];
        };
      });
}
