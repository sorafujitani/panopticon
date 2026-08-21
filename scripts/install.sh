#!/bin/sh
set -eu

umask 077

SCRIPT=$0
case "$SCRIPT" in
  */*) ;;
  *) SCRIPT=$(command -v -- "$SCRIPT") ;;
esac
while [ -L "$SCRIPT" ]; do
  LINK=$(readlink "$SCRIPT")
  case "$LINK" in
    /*) SCRIPT=$LINK ;;
    *) SCRIPT=$(dirname "$SCRIPT")/$LINK ;;
  esac
done
SCRIPT_DIR=$(
  unset CDPATH
  cd -- "$(dirname -- "$SCRIPT")" && pwd -P
)
ROOT_DIR=$(
  unset CDPATH
  cd -- "$SCRIPT_DIR/.." && pwd -P
)
SOURCE="$ROOT_DIR/bin/panopticon"
BIN_DIR=${HOME:?HOME must be set}/.local/bin
DEST="$BIN_DIR/panopticon"

if [ ! -f "$SOURCE" ] || [ ! -x "$SOURCE" ]; then
  printf '%s\n' "panopticon wrapper is not executable: $SOURCE" >&2
  exit 1
fi

mkdir -p "$BIN_DIR"
if [ -L "$DEST" ]; then
  if [ "$(readlink "$DEST")" = "$SOURCE" ]; then
    # The wrapper is stable; refreshing its generated Go binary is safe.
    :
  else
    printf '%s\n' "Do not overwrite the existing symlink: $DEST" >&2
    exit 1
  fi
elif [ -e "$DEST" ]; then
  printf '%s\n' "Do not overwrite the existing file: $DEST" >&2
  exit 1
fi

CACHE="$ROOT_DIR/.panopticon-bin"
temporary="$ROOT_DIR/.panopticon-bin.install.$$"
trap 'rm -f "$temporary"' EXIT HUP INT TERM
(
  unset CDPATH
  cd -- "$ROOT_DIR"
  go build -o "$temporary" ./cmd/panopticon
)
chmod 0700 "$temporary"
mv -f -- "$temporary" "$CACHE"

if [ -L "$DEST" ] && [ "$(readlink "$DEST")" = "$SOURCE" ]; then
  trap - EXIT HUP INT TERM
  printf '%s\n' "Already installed: $DEST -> $SOURCE"
  exit 0
fi

link_temporary="$BIN_DIR/.panopticon.$$"
trap 'rm -f "$temporary" "$link_temporary"' EXIT HUP INT TERM
ln -s "$SOURCE" "$link_temporary"
# mv -n keeps a concurrent destination intact instead of replacing it.
mv -n "$link_temporary" "$DEST"
if [ ! -L "$DEST" ] || [ "$(readlink "$DEST")" != "$SOURCE" ]; then
  printf '%s\n' "Failed to create symlink: $DEST" >&2
  exit 1
fi

trap - EXIT HUP INT TERM
printf '%s\n' "Installed: $DEST -> $SOURCE"
