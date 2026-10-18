{ pkgs, ... }:

{
  # Must be the host user that runs hanare.
  hanare.user = "alice";
  # Keep host-executed config in shares (.git, .claude, .envrc, ...) out of the
  # guest's reach (default true).
  # hanare.protectShares = false;

  environment.systemPackages = with pkgs; [ vim ];
}
