#!/bin/sh
# Build the Rust engine and put its header and static library where the cgo
# directives in store/kura expect them.
#
# The engine is a separate repository on purpose, and there are no releases to
# download yet, so this clones it and builds it. Everything it produces lands
# under third_party, which is ignored by git: a checked in binary is a binary
# nobody can review and a build nobody can reproduce.
set -eu

REPO=${KURA_REPO:-https://github.com/tamnd/kura}
REF=${KURA_REF:-main}
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
SRC=${KURA_SRC:-$ROOT/third_party/kura-src}
DEST=$ROOT/third_party/kura

if ! command -v cargo >/dev/null 2>&1; then
	echo "kura: cargo is not on PATH, and the engine is built from source" >&2
	echo "kura: https://rustup.rs, then run this again" >&2
	exit 1
fi

if [ -d "$SRC/.git" ]; then
	git -C "$SRC" fetch --quiet origin "$REF"
	git -C "$SRC" checkout --quiet FETCH_HEAD
else
	rm -rf "$SRC"
	git clone --quiet --depth 1 --branch "$REF" "$REPO" "$SRC"
fi

echo "kura: building $(git -C "$SRC" rev-parse --short HEAD)"
(cd "$SRC" && cargo build --release -p kura-ffi)

mkdir -p "$DEST/include" "$DEST/lib"
cp "$SRC/include/kura.h" "$DEST/include/"
# Windows names it kura.lib rather than libkura.a, and the loop takes whichever
# one this platform produced rather than guessing from the OS.
for lib in "$SRC"/target/release/libkura.a "$SRC"/target/release/kura.lib; do
	[ -f "$lib" ] && cp "$lib" "$DEST/lib/"
done

echo "kura: $DEST is ready, build with CGO_ENABLED=1 go build -tags kura ./..."
