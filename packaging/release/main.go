// Command release signs Whydah's releases (see scripts/release.sh):
//
//	go run ./packaging/release keygen       make the signing key, once
//	go run ./packaging/release sign FILE    write FILE.sig, FILE's signature
//
// The private key is kept outside the repository, in the user's config
// folder as whydah-release/signing-key (WHYDAH_RELEASE_KEY overrides
// where); keygen writes the
// public key into internal/selfupdate/key.go for builds to check
// signatures with. Back the private key up: without it, no later release
// can be signed, and installed copies won't update themselves.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	var err error
	switch {
	case len(os.Args) == 2 && os.Args[1] == "keygen":
		err = keygen()
	case len(os.Args) == 3 && os.Args[1] == "sign":
		err = sign(os.Args[2])
	default:
		err = errors.New("usage: release keygen | release sign FILE")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func keyPath() (string, error) {
	if p := os.Getenv("WHYDAH_RELEASE_KEY"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "whydah-release", "signing-key"), nil
}

func keygen() error {
	path, err := keyPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists; releases signed with a new key won't install over ones signed with it", path)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return err
	}
	src := fmt.Sprintf(`package selfupdate

// publicKey checks releases' signatures: the ed25519 public key, base64.
// `+"`go run ./packaging/release keygen`"+` wrote it.
var publicKey = %q
`, base64.StdEncoding.EncodeToString(pub))
	if err := os.WriteFile(filepath.Join("internal", "selfupdate", "key.go"), []byte(src), 0o644); err != nil {
		return err
	}
	fmt.Println("signing key:", path, "(back it up; never commit it)")
	fmt.Println("public key written to internal/selfupdate/key.go")
	return nil
}

func sign(file string) error {
	path, err := keyPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("no signing key (run keygen first): %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return fmt.Errorf("%s isn't a signing key", path)
	}
	msg, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	sig := ed25519.Sign(ed25519.NewKeyFromSeed(seed), msg)
	return os.WriteFile(file+".sig", []byte(base64.StdEncoding.EncodeToString(sig)+"\n"), 0o644)
}
