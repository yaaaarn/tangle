{
  lib,
  buildGoModule,
}:
buildGoModule {
  pname = "tangle";
  version = "unstable";

  src = ./.;

  vendorHash = "sha256-g7tMp2rWtAoWlx8NrWcwcKJpw7DFMtJzH5OLG8fsWlU=";

  meta = with lib; {
    description = "a battery monitor utility";
    homepage = "https://github.com/yaaaarn/tangle";
    license = licenses.mit;
    mainProgram = "tangle";
  };
}
