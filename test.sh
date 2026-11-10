#!/bin/sh
# go.local.mod points the pig SDK requirement at the SDK pig staged on this
# machine, so the tests run against the exact copy pig builds the extension with
# (go.mod pins a released version, which is what editors resolve).
# It is generated under .cache so nothing lands in the package root.
set -eu

sdk=""
for candidate in "$HOME"/.pig/state/pigsdk/sdk-[0-9a-f]*; do
	if [ -d "$candidate" ]; then
		sdk="$candidate"
	fi
done

if [ -z "$sdk" ]; then
	echo "error: no staged pig sdk under ~/.pig/state/pigsdk" >&2
	echo "run: pig extension init --lang go ." >&2
	exit 1
fi

mkdir -p .cache
{
	cat go.mod
	printf '\nreplace github.com/MichaelKinsy/PiG/extensions/sdk => %s\n' "$sdk"
} > .cache/go.local.mod
cp go.sum .cache/go.local.sum

# -mod=readonly on purpose: pig builds this package from a generated module that
# only carries the requirements declared in go.mod, so a missing indirect
# requirement passes a permissive local build and then fails at install time.
# Keeping the local modfile read-only makes that failure happen here instead.
GOFLAGS=-mod=readonly go vet -modfile=.cache/go.local.mod ./...
GOFLAGS=-mod=readonly go test -modfile=.cache/go.local.mod ./...
