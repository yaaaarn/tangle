{
  lib,
  buildGoModule,
}:
buildGoModule {
  pname = "tangle";
  version = "unstable";

  src = ./.;

  vendorHash = "sha256-g+yaVIx4jxpAQ/+WrGKxhVeliYx7nLQe/zsGpxV4Fn4=";

  meta = with lib; {
    description = "a battery monitor utility";
    homepage = "https://github.com/yaaaarn/tangle";
    license = licenses.mit;
    mainProgram = "tangle";
  };
}
