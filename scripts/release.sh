#!/bin/sh
# Builds, signs and publishes a release of Whydah as a GitHub release, for
# installed copies to update themselves from (internal/selfupdate). Run it
# as `make release`; `make release DRY=1` (--dry-run) builds and signs
# everything into dist/ without tagging or publishing.
#
# It needs an Apple Developer ID Application certificate and notarytool
# credentials (the "whydah-notary" keychain profile), so macOS opens the
# downloads without asking and keeps their permissions from release to
# release; the release signing key (go run ./packaging/release keygen); and,
# to publish, the GitHub CLI (gh) and a clean, pushed working tree. See
# README, "Releasing".
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
sign_id="${SIGN_ID:-$(security find-identity -v -p codesigning | awk '/"Developer ID Application:/ {print $2; exit}')}"
[ -n "$sign_id" ] || fail "no Developer ID Application certificate in the keychain (see README, \"Releasing\")"
notary="${NOTARY_PROFILE:-whydah-notary}"
xcrun notarytool history --keychain-profile "$notary" >/dev/null 2>&1 || fail "no \"$notary\" notarytool credentials (see README, \"Releasing\")"
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

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
sign_flags="--options runtime --timestamp"
make dist VERSION="$version"
make app VERSION="$version" SIGN_ID="$sign_id" SIGN_FLAGS="$sign_flags"
codesign --force $sign_flags --sign "$sign_id" dist/whydah-darwin-arm64 dist/whydah-darwin-amd64

# Apple notarizes the app and the macOS programs in one submission. The app
# gets the ticket stapled to it, so it opens without asking even offline; a
# bare program can't hold one, and macOS looks its ticket up online.
echo "notarizing (usually a few minutes)"
mkdir "$tmp/notarize"
ditto bin/Whydah.app "$tmp/notarize/Whydah.app"
cp dist/whydah-darwin-arm64 dist/whydah-darwin-amd64 "$tmp/notarize/"
ditto -c -k --keepParent "$tmp/notarize" "$tmp/notarize.zip"
result=$(xcrun notarytool submit "$tmp/notarize.zip" --keychain-profile "$notary" --wait --output-format json) || fail "notarytool: $result"
if ! echo "$result" | grep -Eq '"status" *: *"Accepted"'; then
	id=$(echo "$result" | sed -nE 's/.*"id" *: *"([^"]+)".*/\1/p')
	[ -n "$id" ] && xcrun notarytool log "$id" --keychain-profile "$notary" >&2
	fail "Apple didn't notarize the build: $result"
fi
xcrun stapler staple bin/Whydah.app
spctl --assess --type execute bin/Whydah.app || fail "macOS won't open the notarized app"
ditto -c -k --keepParent bin/Whydah.app dist/Whydah-macos.zip
(cd dist && shasum -a 256 whydah-* Whydah-macos.zip >SHA256SUMS)
go run ./packaging/release sign dist/SHA256SUMS

if [ $publish = 0 ]; then
	echo "dry run: the release is in dist/, not published"
	exit 0
fi
# Which file to download goes above the generated list of changes.
notes=$tmp/notes.md
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

**The first time you open it:** the macOS builds are signed and notarized by Apple and open straight away. The Windows builds aren't signed by Microsoft, so Windows may warn first: choose *More info* → *Run anyway*. On Linux, make it runnable first: `chmod +x whydah-linux-*`. After that, Whydah offers its own updates and installs them without asking again.

NOTES

git tag "$version"
git push origin "$version"
gh release create "$version" dist/* --title "Whydah $version" --notes-file "$notes" --generate-notes
