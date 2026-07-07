{ lib, buildGoModule }:

buildGoModule {
  pname = "msg";
  version = "0-unstable";
  src = ./.;

  # vendorHash can't be known before the first build. Leave lib.fakeHash, run
  # `nix build .#msg`, and paste the "got: sha256-..." value it prints.
  vendorHash = "sha256-np9NRRIC03CbzcMKzrpHesUkFCbZphnxLWRPcCp/4R0=";

  meta = {
    description = "XMPP/Jabber messaging CLI";
    homepage = "https://github.com/zachpmanson/msg";
    mainProgram = "msg";
  };
}
