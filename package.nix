{ lib, buildGoModule }:

buildGoModule {
  pname = "msg";
  version = "0-unstable";
  src = ./.;

  # vendorHash can't be known before the first build. Leave lib.fakeHash, run
  # `nix build .#msg`, and paste the "got: sha256-..." value it prints.
  vendorHash = "sha256-0sHUqGUvafjKPJamk8AqOZ1q8C6172ZEuiSDTypogp8=";

  meta = {
    description = "XMPP/Jabber messaging CLI";
    homepage = "https://github.com/zachpmanson/msg";
    mainProgram = "msg";
  };
}
