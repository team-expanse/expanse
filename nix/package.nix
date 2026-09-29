{ lib, buildGoModule, version, rev }:
buildGoModule {
  pname = "expanse";
  inherit version;
  src = lib.cleanSource ../.;
  vendorHash = null; # deps are vendored in ./vendor
  env.CGO_ENABLED = "0";
  ldflags = [
    "-s" "-w"
    "-X github.com/expanse/expanse/internal/version.Version=${version}"
    "-X github.com/expanse/expanse/internal/version.Commit=${rev}"
  ];
  # The block runtime helper (Phase 04) builds alongside expanse and is
  # installed next to it at /run/current-system/sw/bin.
  subPackages = [ "cmd/expanse" "cmd/expanse-block-run" ];

  # Ship LICENSE, NOTICE and each vendored module's licence with the binaries (MIT/BSD require it).
  postInstall = ''
    dir=$out/share/licenses/expanse
    install -Dm644 -t $dir LICENSE NOTICE
    {
      echo "# Third-party Go modules in expanse ${version}"
      echo
      echo "Expanse vendors these Go modules and builds its binaries from them. Each one's licence is in"
      echo "vendor/<module>/ here; their source is in the Expanse source tree under vendor/."
      echo
      echo "| Module | Version |"
      echo "|---|---|"
    } > $dir/THIRD-PARTY.md
    while read -r hash mod ver _; do
      [ "$hash" = "#" ] && [ "$mod" != "=>" ] || continue
      echo "| $mod | $ver |" >> $dir/THIRD-PARTY.md
      for f in vendor/$mod/*; do
        case ''${f##*/} in LICEN[CS]E* | [Ll]icen[cs]e* | COPYING* | NOTICE*) install -Dm644 "$f" "$dir/vendor/$mod/''${f##*/}" ;; esac
      done
    done < vendor/modules.txt
  '';

  meta = {
    license = lib.licenses.asl20;
    mainProgram = "expanse";
  };
}