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
SOURCE="$ROOT_DIR/skills/panopticon"

if [ ! -f "$SOURCE/SKILL.md" ]; then
  printf '%s\n' "SKILL.md not found: $SOURCE/SKILL.md" >&2
  exit 1
fi

TARGET_ROOT=${PANOPTICON_SKILL_DIR:-${HOME:?HOME must be set}/.agents/skills}
DEST="$TARGET_ROOT/panopticon"

mkdir -p "$TARGET_ROOT"
if [ -L "$DEST" ]; then
  if [ "$(readlink "$DEST")" = "$SOURCE" ]; then
    printf '%s\n' "Already installed: $DEST -> $SOURCE"
    exit 0
  fi
  printf '%s\n' "Do not overwrite the existing symlink: $DEST" >&2
  exit 1
elif [ -e "$DEST" ]; then
  printf '%s\n' "Do not overwrite the existing file: $DEST" >&2
  exit 1
fi

link_temporary="$TARGET_ROOT/.panopticon-skill.$$"
trap 'rm -f "$link_temporary"' EXIT HUP INT TERM
ln -s "$SOURCE" "$link_temporary"
# mv -n keeps a concurrent destination intact instead of replacing it.
mv -n "$link_temporary" "$DEST"
if [ ! -L "$DEST" ] || [ "$(readlink "$DEST")" != "$SOURCE" ]; then
  printf '%s\n' "Failed to create symlink: $DEST" >&2
  exit 1
fi

trap - EXIT HUP INT TERM
printf '%s\n' "Installed: $DEST -> $SOURCE"
