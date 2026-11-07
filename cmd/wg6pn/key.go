package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/curve25519"
)

func loadOrCreateKey(path string) (pub, priv string, created bool, err error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		pub, priv, err = parseKey(b)
		if err != nil {
			return "", "", false, fmt.Errorf("%s: %w", path, err)
		}
		return pub, priv, false, nil
	case !errors.Is(err, fs.ErrNotExist):
		return "", "", false, err
	}

	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", "", false, err
	}
	priv = base64.StdEncoding.EncodeToString(k[:])

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", "", false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".key-*")
	if err != nil {
		return "", "", false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(priv + "\n"); err != nil {
		tmp.Close()
		return "", "", false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", "", false, err
	}
	if err := tmp.Close(); err != nil {
		return "", "", false, err
	}
	// Link refuses to overwrite, so concurrent first runs agree on one key.
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			pub, priv, _, err := loadOrCreateKey(path)
			return pub, priv, false, err
		}
		return "", "", false, err
	}

	pub, priv, err = parseKey([]byte(priv))
	return pub, priv, true, err
}

func parseKey(b []byte) (pub, priv string, err error) {
	priv = strings.TrimSpace(string(b))
	raw, err := base64.StdEncoding.DecodeString(priv)
	if err != nil || len(raw) != 32 {
		return "", "", errors.New("not a base64 32-byte WireGuard private key")
	}
	p, err := curve25519.X25519(raw, curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(p), priv, nil
}
