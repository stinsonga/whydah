#!/bin/sh
# Creates a self-signed code-signing certificate, "Whydah Local Signing", in
# your login keychain, for `make app`. macOS ties privacy permissions (like
# Full Disk Access) to the app's signature; signing every build with the same
# certificate keeps them across rebuilds. Only meant for this Mac: other Macs
# don't trust it. Remove it any time in Keychain Access → My Certificates.
set -eu
name="Whydah Local Signing"
keychain="$HOME/Library/Keychains/login.keychain-db"

if security find-identity -p codesigning "$keychain" | grep -q "$name"; then
	echo "\"$name\" already exists."
	exit 0
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cat > "$tmp/cert.cnf" <<CNF
[req]
distinguished_name = dn
x509_extensions = ext
prompt = no
[dn]
CN = $name
[ext]
basicConstraints = critical, CA:false
keyUsage = critical, digitalSignature
extendedKeyUsage = critical, codeSigning
CNF
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -config "$tmp/cert.cnf" \
	-keyout "$tmp/key.pem" -out "$tmp/cert.pem" 2>/dev/null
pass=$(openssl rand -hex 16)
# OpenSSL 3 (e.g. Homebrew's) needs -legacy for a file macOS can read;
# Apple's LibreSSL doesn't know the flag and already writes the old format.
openssl pkcs12 -export -legacy -inkey "$tmp/key.pem" -in "$tmp/cert.pem" -name "$name" \
	-passout "pass:$pass" -out "$tmp/cert.p12" 2>/dev/null || \
openssl pkcs12 -export -inkey "$tmp/key.pem" -in "$tmp/cert.pem" -name "$name" \
	-passout "pass:$pass" -out "$tmp/cert.p12"
security import "$tmp/cert.p12" -k "$keychain" -P "$pass" -T /usr/bin/codesign
echo "Created \"$name\". Now run: make app"
