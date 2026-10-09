# curl 8.22 drops cookies it reads back from a jar for single-label hosts such as n1
# (curl/curl#23261); the UI tests keep sessions in jars, so they use curl without PSL.
{ pkgs, lib, ... }:
{
  environment.systemPackages = [ (lib.hiPrio (pkgs.curl.override { pslSupport = false; })) ];
}
