{ lib, buildGoModule, version ? "0.1.0", targetOs ? null, targetArch ? null }:

let
  isCross = targetOs != null || targetArch != null;
in
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

  # `buildGoModule` defaults GOOS/GOARCH from hostPlatform; override both in
  # `preBuild` (after stdenv/go setup hooks) so cross-targets emit real
  # OpenBSD binaries while keeping the shared vendored `goModules` derivation.
  env.CGO_ENABLED = 0;

  preBuild = lib.optionalString isCross (
    lib.optionalString (targetOs != null) ''
      export GOOS=${lib.escapeShellArg targetOs}
    '' + lib.optionalString (targetArch != null) ''
      export GOARCH=${lib.escapeShellArg targetArch}
    ''
  );

  doCheck = !isCross;

  # Foreign-arch Go binaries (e.g. aarch64 built on x86_64) cannot be stripped
  # by the build host's native binutils strip, and OpenBSD ELF headers must
  # not be touched by Linux fixups.
  dontStrip = isCross;
  dontPatchELF = isCross;

  # `go install` writes cross-compiled binaries under $out/bin/${GOOS}_${GOARCH}/;
  # promote to $out/bin/ripws so `nix build .#ripws-openbsd-amd64` behaves uniformly.
  postInstall = lib.optionalString isCross ''
    for d in "$out"/bin/*_*; do
      if [ -d "$d" ] && [ -e "$d/ripws" ]; then
        mv "$d/ripws" "$out/bin/ripws"
        rmdir "$d"
      fi
    done
  '';

  meta = with lib; {
    description = "Raw IP over WebSocket tunnel";
    homepage = "https://github.com/kenh0u/ripws";
    license = licenses.mit;
    mainProgram = "ripws";
    platforms = platforms.linux ++ platforms.openbsd;
  };
}
