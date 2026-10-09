# Pins the DRBD kernel module to 9.2.16, the release every storage test passed on;
# nixpkgs' 9.3.3 refuses kernel 6.18.
{
  nixpkgs.overlays = [
    (final: prev: {
      linuxPackages = prev.linuxPackages.extend (_: kprev: {
        drbd = kprev.drbd.overrideAttrs (old: {
          version = "9.2.16";
          src = final.fetchurl {
            url = "https://pkg.linbit.com//downloads/drbd/9/drbd-9.2.16.tar.gz";
            hash = "sha256-2ff9XtSlUnJG5y6qrRYGTgQiZdEnzywKaKR96ItF8Zw=";
          };
          meta = old.meta // { broken = false; };
        });
      });
    })
  ];
}
