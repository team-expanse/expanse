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
      imageDigest = "sha256:b0012818446ff99595858ce6b2de25522803de552a95230f396d76367fc44212";
      hash = "sha256-k6QA/Ihy4hiU9rrGndwZJRd1YOvSXAlZ4xolNxCXp6M=";
      finalImageName = "trypando/pando";
      finalImageTag = "0.12.0";
    }
    {
      imageName = "postgres";
      imageDigest = "sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24";
      hash = "sha256-8muMAZ/8WS+XtAiUqVclbM54NcBrMIB04emhZZPev/I=";
      finalImageName = "postgres";
      finalImageTag = "17.11-alpine";
    }
  ]
