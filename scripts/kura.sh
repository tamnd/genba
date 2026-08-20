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
# Pinned, because the engine is a separate repository and a build that tracks a
# branch is not reproducible: two machines a day apart get two different
# engines, and a commit over there turns CI red on whichever pull request here
# happens to be open. Bumping this is a commit like any other, which is what
# makes the tests run against the new engine before it lands rather than after.
REF=${KURA_REF:-bab2997c0a373d04e48a16c89c68a30dc3039e6e}
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
SRC=${KURA_SRC:-$ROOT/third_party/kura-src}
DEST=$ROOT/third_party/kura

if ! command -v cargo >/dev/null 2>&1; then
	echo "kura: cargo is not on PATH, and the engine is built from source" >&2
	echo "kura: https://rustup.rs, then run this again" >&2
	exit 1
fi

# One path for both cases, because --branch takes a name and REF is a commit.
# An empty clone followed by a fetch of the ref is what works for a commit, a
# tag and a branch alike, so an override does not have to be any particular one
# of those.
if [ ! -d "$SRC/.git" ]; then
	rm -rf "$SRC"
	mkdir -p "$SRC"
	git -C "$SRC" init --quiet
	git -C "$SRC" remote add origin "$REPO"
fi
git -C "$SRC" fetch --quiet --depth 1 origin "$REF"
git -C "$SRC" checkout --quiet --force FETCH_HEAD

# On Windows, cargo defaults to the MSVC toolchain and cgo uses the mingw gcc
# that comes with Go. A library from one does not link into the other, and the
# error when it does not is a page of undefined symbols that says nothing about
# toolchains. So Windows builds the gnu target, which produces the libkura.a
# that mingw expects.
TARGET=""
OUT="$SRC/target/release"
case $(uname -s) in
MINGW* | MSYS* | CYGWIN*)
	TARGET=x86_64-pc-windows-gnu
	OUT="$SRC/target/$TARGET/release"
	# The standard library for that target has to be installed, and the error
	# when it is not is "can't find crate for std", which says nothing about
	# which toolchain is missing. So this runs loudly rather than being tried
	# and ignored.
	if ! command -v rustup >/dev/null 2>&1; then
		echo "kura: rustup is not on PATH, and the $TARGET standard library has to come from somewhere" >&2
		exit 1
	fi
	# Inside the source tree, because the engine pins its toolchain in a
	# rust-toolchain.toml and rustup only reads that from the directory it is
	# run in. Adding the target anywhere else adds it to whichever toolchain
	# happens to be the default, which is not the one cargo is about to use, and
	# the build then fails with exactly the error this is here to prevent.
	(cd "$SRC" && rustup target add "$TARGET")
	;;
esac

echo "kura: building $(git -C "$SRC" rev-parse --short HEAD) ${TARGET:-for this host}"
if [ -n "$TARGET" ]; then
	(cd "$SRC" && cargo build --release -p kura-ffi --target "$TARGET")
else
	(cd "$SRC" && cargo build --release -p kura-ffi)
fi

mkdir -p "$DEST/include" "$DEST/lib"
cp "$SRC/include/kura.h" "$DEST/include/"
found=""
for lib in "$OUT/libkura.a" "$OUT/kura.lib"; do
	if [ -f "$lib" ]; then
		cp "$lib" "$DEST/lib/"
		found=$lib
	fi
done
if [ -z "$found" ]; then
	echo "kura: cargo produced no static library in $OUT" >&2
	exit 1
fi

echo "kura: $DEST is ready, build with CGO_ENABLED=1 go build -tags kura ./..."
