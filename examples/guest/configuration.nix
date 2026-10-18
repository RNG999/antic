{ pkgs, ... }:

{
  # Must be the host user that runs hanare.
  hanare.user = "alice";
  # Both default to true:
  # hanare.restrictUserNamespaces = false;  # deny user namespaces to non-root (BPF LSM)
  # hanare.audit = false;                   # activity log written to the host

  environment.systemPackages = with pkgs; [ git ];
}
