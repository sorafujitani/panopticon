#!/bin/sh
set -eu

umask 077

SCRIPT_DIR=$(
    unset CDPATH
    cd -- "$(dirname -- "$0")" && pwd -P
)
SOURCE_DIR=$(
    unset CDPATH
    cd -- "$SCRIPT_DIR/../bin" && pwd -P
)
SOURCE="$SOURCE_DIR/panopticon"
BIN_DIR=${HOME:?HOME が設定されていません}/.local/bin
DEST="$BIN_DIR/panopticon"

if [ ! -f "$SOURCE" ] || [ ! -x "$SOURCE" ]; then
    printf '%s\n' "panopticon wrapper が実行可能ではありません: $SOURCE" >&2
    exit 1
fi

mkdir -p "$BIN_DIR"

if [ -L "$DEST" ]; then
    if [ "$(readlink "$DEST")" = "$SOURCE" ]; then
        printf '%s\n' "既にインストール済みです: $DEST -> $SOURCE"
        exit 0
    fi
    printf '%s\n' "既存の symlink を上書きしません: $DEST" >&2
    exit 1
fi
if [ -e "$DEST" ]; then
    printf '%s\n' "既存のファイルを上書きしません: $DEST" >&2
    exit 1
fi

temporary="$BIN_DIR/.panopticon.$$"
trap 'rm -f "$temporary"' EXIT HUP INT TERM
ln -s "$SOURCE" "$temporary"

# mv -n は競合時に既存の destination を上書きしない。
mv -n "$temporary" "$DEST"
if [ ! -L "$DEST" ] || [ "$(readlink "$DEST")" != "$SOURCE" ]; then
    printf '%s\n' "symlink の作成に失敗しました: $DEST" >&2
    exit 1
fi

trap - EXIT HUP INT TERM
printf '%s\n' "インストールしました: $DEST -> $SOURCE"
