# The container images compose.yaml names, pinned by digest (nix-prefetch-docker).
{ pkgs }:
map
  (i: {
    ref = "${i.finalImageName}:${i.finalImageTag}";
    tarball = pkgs.dockerTools.pullImage (i // { os = "linux"; arch = "amd64"; });
  })
  [
    {
      imageName = "trypando/pando";
      imageDigest = "sha256:624fdd622c95b05a58c21e3a9dfd4944d7326db98ea82041ada053ae0634187c";
      hash = "sha256-LexVmouL2IC5JAdX4tg8TKDMqQl4QXEnuXhLy/3Gf7M=";
      finalImageName = "trypando/pando";
      finalImageTag = "0.10.0";
    }
    {
      imageName = "postgres";
      imageDigest = "sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24";
      hash = "sha256-8muMAZ/8WS+XtAiUqVclbM54NcBrMIB04emhZZPev/I=";
      finalImageName = "postgres";
      finalImageTag = "17.11-alpine";
    }
  ]
