#!/bin/sh
# Builds, signs and publishes a release of Whydah as a GitHub release, for
# installed copies to update themselves from (internal/selfupdate). Run it
# as `make release`; `make release DRY=1` (--dry-run) builds and signs
# everything into dist/ without tagging or publishing.
#
# It needs the macOS signing certificate (scripts/make-signing-cert.sh), so
# every release's app is signed alike and macOS permissions carry over; the
# release signing key (go run ./packaging/release keygen); and, to publish,
# the GitHub CLI (gh) and a clean, pushed working tree.
set -eu
cd "$(dirname "$0")/.."

publish=1
if [ "${1:-}" = "--dry-run" ]; then
	publish=0
fi
fail() {
	echo "release: $*" >&2
	exit 1
}

grep -q 'publicKey = ""' internal/selfupdate/key.go && fail "no signing key yet: run go run ./packaging/release keygen, then commit internal/selfupdate/key.go"
sign_id="${SIGN_ID:-Whydah Local Signing}"
security find-identity -p codesigning | grep -q "$sign_id" || fail "no \"$sign_id\" certificate: run scripts/make-signing-cert.sh"
if [ $publish = 1 ]; then
	command -v gh >/dev/null || fail "publishing needs the GitHub CLI (gh)"
	[ -z "$(git status --porcelain)" ] || fail "commit your changes first: a release is built from a tagged commit"
	git fetch -q origin || fail "can't reach origin to check this commit is pushed"
	git merge-base --is-ancestor HEAD '@{upstream}' 2>/dev/null || fail "push first: this commit isn't on $(git rev-parse --abbrev-ref '@{upstream}' 2>/dev/null || echo the remote)"
fi

# The version is the date, with a count for a second release the same day.
day=$(date +%Y.%m.%d)
version=$day
n=1
while git rev-parse -q --verify "refs/tags/$version" >/dev/null ||
	{ [ $publish = 1 ] && gh release view "$version" >/dev/null 2>&1; }; do
	n=$((n + 1))
	version=$day.$n
done
echo "Whydah $version"

make dist VERSION="$version"
make app VERSION="$version" SIGN_ID="$sign_id"
codesign --force --sign "$sign_id" dist/whydah-darwin-arm64 dist/whydah-darwin-amd64
ditto -c -k --keepParent bin/Whydah.app dist/Whydah-macos.zip
(cd dist && shasum -a 256 whydah-* Whydah-macos.zip >SHA256SUMS)
go run ./packaging/release sign dist/SHA256SUMS

if [ $publish = 0 ]; then
	echo "dry run: the release is in dist/, not published"
	exit 0
fi
# Which file to download goes above the generated list of changes.
notes=$(mktemp)
trap 'rm -f "$notes"' EXIT
cat >"$notes" <<'NOTES'
## Which file do I need?

| System | Download |
| --- | --- |
| macOS (Apple Silicon or Intel) | `Whydah-macos.zip`: unzip it and move Whydah.app to Applications |
| Windows (most PCs) | `whydah-windows-amd64.exe` |
| Windows on ARM | `whydah-windows-arm64.exe` |
| Linux (most PCs) | `whydah-linux-amd64` |
| Linux on ARM (Raspberry Pi and the like) | `whydah-linux-arm64` |

`whydah-darwin-*` are the macOS command-line programs without the app. `SHA256SUMS` and `SHA256SUMS.sig` are what Whydah checks its updates against.

**The first time you open it:** these builds aren't signed by Apple or Microsoft, so your system asks first. On macOS, open Whydah once, then allow it in System Settings → Privacy & Security (*Open Anyway*). On Windows, choose *More info* → *Run anyway*. On Linux, make it runnable first: `chmod +x whydah-linux-*`. After that, Whydah offers its own updates and installs them without asking again.

NOTES

git tag "$version"
git push origin "$version"
gh release create "$version" dist/* --title "Whydah $version" --notes-file "$notes" --generate-notes
