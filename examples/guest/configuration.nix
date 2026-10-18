{ pkgs, ... }:

{
  # Must be the host user that runs hanare.
  hanare.user = "alice";
  # hanare.allowLan = true;
  # hanare.protectGit = false;

  environment.systemPackages = with pkgs; [ vim ];
}
