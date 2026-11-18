{ lib, buildGoModule, version ? "0.1.0" }:

buildGoModule {
  pname = "ripws";
  inherit version;

  src = lib.cleanSourceWith {
    src = lib.cleanSource ../.;
    # Only Go sources matter for the build; keep VM-test scratch files out.
    filter = path: _type:
      let base = baseNameOf path;
      in !(lib.hasSuffix ".test" base || base == "result" || base == "result-link");
  };

  # `subPackages` defaults to "."; we only have the root package as
  # the binary entry point.
  subPackages = [ "." ];

  # Computed with `nix build` (lib.fakeHash then read the got hash).
  vendorHash = "sha256-2oHinTZ5MU464sDS9faZdGC7ggiKLac8PR9Xod9OcyY=";

  # Pure-Go static build so the binary runs in any minimal container
  # (no nix glibc loader needed).
  env.CGO_ENABLED = 0;

  doCheck = true;

  meta = with lib; {
    description = "Raw IP over WebSocket tunnel";
    homepage = "https://github.com/kenh0u/ripws";
    license = licenses.mit;
    mainProgram = "ripws";
    platforms = platforms.linux;
  };
}
