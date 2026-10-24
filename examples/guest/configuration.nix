{ pkgs, ... }:

{
  # Must be the host user that runs antic.
  antic.user = "alice";

  environment.systemPackages = with pkgs; [ git ];
}
