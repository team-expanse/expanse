# Pins the DRBD kernel module to 9.2.20, the release every storage test passed on;
# nixpkgs' 9.3.3 refuses kernel 6.18.
{
  nixpkgs.overlays = [
    (final: prev: {
      linuxPackages = prev.linuxPackages.extend (_: kprev: {
        drbd = kprev.drbd.overrideAttrs (old: {
          version = "9.2.20";
          src = final.fetchurl {
            url = "https://pkg.linbit.com//downloads/drbd/9/drbd-9.2.20.tar.gz";
            hash = "sha256-Sm7NBIzUGu9kJ7mb/LKxThbzbwseFibldR00Xs0rCWI=";
          };
          # nixpkgs substitutes builtins.getEnv "SHELL", so each evaluator got its own drv and the
          # release cache never matched what a node asked for.
          postPatch = ''
            patchShebangs .
            substituteInPlace Makefile --replace-fail 'SHELL=/bin/bash' 'SHELL=${final.runtimeShell}'
            # 6.18.57 backported sockaddr_unsized to bind() only; DRBD assumes connect() changed too.
            substituteInPlace drbd/drbd_transport_tcp.c \
              --replace-fail 'socket->ops->connect(socket, (struct sockaddr_unsized *)' 'socket->ops->connect(socket, (struct sockaddr *)'
            substituteInPlace drbd/drbd_transport_lb-tcp.c \
              --replace-fail 'sock->ops->connect(sock, (struct sockaddr_unsized *)' 'sock->ops->connect(sock, (struct sockaddr *)'
          '';
          meta = old.meta // { broken = false; };
        });
      });
    })
  ];
}
