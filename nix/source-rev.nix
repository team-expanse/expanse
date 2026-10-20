# The commit an expanse source tree was archived from, or `fallback` for a checkout or a flake copy.
{ fallback, file ? ./source-rev }:
let rev = builtins.replaceStrings [ "\n" ] [ "" ] (builtins.readFile file);
in if builtins.substring 0 1 rev == "$" then fallback else rev
