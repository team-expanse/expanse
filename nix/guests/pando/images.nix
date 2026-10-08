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
      imageDigest = "sha256:edab56e28e1484edfc1d1c1059fb45cdf82a229fe4daf56fe3088addbaaa8eb8";
      hash = "sha256-J01mmfd1a7bD8i5/JgFB99oFtxtKfTU81+bQeh+TdfY=";
      finalImageName = "trypando/pando";
      finalImageTag = "0.3.0";
    }
    {
      imageName = "postgres";
      imageDigest = "sha256:b0f9560a2de083e2cc7382e75f808c7381a32852a7ec49117deedb300e552b24";
      hash = "sha256-WbUFmk1vvoPwDyoGlMq5nL1h2W4QXd0zeCxUOEXrEnU=";
      finalImageName = "postgres";
      finalImageTag = "17-alpine";
    }
    {
      imageName = "moby/buildkit";
      imageDigest = "sha256:5b45405a38c579692f6fcd47ceef2002fe4fa61bb04ef0c2c644cf74cbbd57b8";
      hash = "sha256-ve4HyzVngtcYBNjfzd5bzmLsB6EE2V2YhZGDnM8KenE=";
      finalImageName = "moby/buildkit";
      finalImageTag = "v0.17.2-rootless";
    }
  ]
