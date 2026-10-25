#!/usr/bin/env sh
set -eu

if [ "$(id -u)" -eq 0 ]; then
  echo "antic: please run install.sh as your normal user, not root." >&2
  exit 1
fi

if [ -z "${HOME:-}" ]; then
  echo "antic: \$HOME is not set." >&2
  exit 1
fi

has_cmd() {
  command -v "$1" >/dev/null 2>&1
}

# 1. Locate nix
if has_cmd nix; then
  NIX_BIN="$(command -v nix)"
elif [ -x "$HOME/.local/bin/nix" ]; then
  NIX_BIN="$HOME/.local/bin/nix"
else
  echo "antic: nix is not installed. Please install Nix first and re-run." >&2
  exit 1
fi

export NIX_CONFIG="extra-experimental-features = nix-command flakes"

# 2. Ensure ~/.config/nix/nix.conf is configured idempotently
NIX_CONF_DIR="$HOME/.config/nix"
NIX_CONF="$NIX_CONF_DIR/nix.conf"

if [ ! -f "$NIX_CONF" ] || ! grep -Eq '^[[:space:]]*experimental-features[[:space:]]*=.*flakes' "$NIX_CONF"; then
  mkdir -p "$NIX_CONF_DIR"
  printf "experimental-features = nix-command flakes\n" >> "$NIX_CONF"
  echo "antic: enabled nix-command and flakes in $NIX_CONF" >&2
fi

if ! "$NIX_BIN" store ping >/dev/null 2>&1; then
  mkdir -p "$NIX_CONF_DIR"
  if ! grep -Eq '^[[:space:]]*store[[:space:]]*=' "$NIX_CONF"; then
    STORE_ROOT="$HOME/.local/share/nix-root"
    printf "store = local?root=%s\n" "$STORE_ROOT" >> "$NIX_CONF"
    echo "antic: configured rootless Nix store in $NIX_CONF ($STORE_ROOT)" >&2
  fi
fi

# 3. Ensure antic directories exist
mkdir -p "$HOME/.config/antic/guests" "$HOME/.local/state/antic"

# 4. Build and install antic and helper binaries (cloud-hypervisor, virtiofsd, passt, pasta, nft)
if [ -n "${ANTIC_SOURCE:-}" ]; then
  SOURCE="$ANTIC_SOURCE"
elif [ -f "$0" ] && [ -f "$(dirname "$0")/flake.nix" ] && [ -f "$(dirname "$0")/cmd/antic/main.go" ]; then
  SOURCE="$(cd "$(dirname "$0")" && pwd)"
else
  SOURCE="github:RNG999/antic"
fi

FLAKE_REF="$SOURCE"
case "$SOURCE" in
  /*|./*|../*)
    SOURCE="$(cd "$SOURCE" && pwd)"
    FLAKE_REF="path:$SOURCE"
    ;;
esac

echo "antic: installing antic and helper binaries from $SOURCE..." >&2
"$NIX_BIN" run "$FLAKE_REF" -- update --source "$SOURCE"

echo "" >&2
"$HOME/.local/bin/antic" doctor

case ":${PATH:-}:" in
  *":$HOME/.local/bin:"*) ;;
  *)
    echo "" >&2
    echo "antic: note: add $HOME/.local/bin to your PATH:" >&2
    echo "  export PATH=\"\$HOME/.local/bin:\$PATH\"" >&2
    ;;
esac
