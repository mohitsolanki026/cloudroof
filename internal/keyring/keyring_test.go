// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

package keyring

import (
	"path/filepath"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	k, err := Open(filepath.Join(dir, "master.key"), "")
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte(`{"privateKey":"-----BEGIN OPENSSH PRIVATE KEY-----\nabc"}`)
	sealed, err := k.Seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(sealed) == string(secret) || len(sealed) == 0 {
		t.Fatal("sealed blob looks like plaintext")
	}
	got, err := k.OpenSealed(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(secret) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
}

func TestOpenPersistsKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	k1, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := k1.Seal([]byte("hello"))
	// A second Open must load the same key from disk and decrypt the blob.
	k2, err := Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := k2.OpenSealed(sealed)
	if err != nil || string(got) != "hello" {
		t.Fatalf("reopened key cannot decrypt: %q %v", got, err)
	}
}

func TestWrongKeyFails(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	k1, _ := Open(filepath.Join(d1, "master.key"), "")
	sealed, _ := k1.Seal([]byte("secret"))
	// A different data dir means a different master key — the blob must not open.
	k2, _ := Open(filepath.Join(d2, "master.key"), "")
	if _, err := k2.OpenSealed(sealed); err == nil {
		t.Fatal("a blob decrypted under the wrong master key")
	}
}

func TestTamperedBlobFails(t *testing.T) {
	dir := t.TempDir()
	k, _ := Open(filepath.Join(dir, "master.key"), "")
	sealed, _ := k.Seal([]byte("secret"))
	// Flip a byte in the middle of the base64 ciphertext.
	b := []byte(sealed)
	b[len(b)/2] ^= 0x01
	if _, err := k.OpenSealed(string(b)); err == nil {
		t.Fatal("GCM accepted a tampered blob")
	}
}
