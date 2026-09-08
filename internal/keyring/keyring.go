// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Mohit Solanki

// Package keyring seals and opens secrets at rest.
//
// CloudRoof stores SSH private keys and cloud API tokens. Those are the crown
// jewels of a user's fleet, so they are encrypted with AES-256-GCM under a
// master key that lives outside the database. The database alone is useless to
// an attacker; they need the key file too.
//
// Plaintext secrets exist only in memory, for the duration of a connection.
package keyring

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	keyLen = 32 // AES-256
	// prefix tags sealed blobs so a future key-rotation or algorithm change can
	// tell versions apart without guessing.
	prefix = "bsn1:"
)

var ErrNoKey = errors.New("keyring: master key not found")

type Keyring struct {
	aead cipher.AEAD
}

// Open loads the master key, preferring an explicit environment value over the
// key file. If neither exists, a new key is generated and written to path.
//
// The env override exists for container users who inject secrets rather than
// mounting files; the file is the default because environment variables leak
// into process listings and shell history.
func Open(path, envValue string) (*Keyring, error) {
	key, err := loadOrCreate(path, envValue)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("keyring: init cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keyring: init gcm: %w", err)
	}
	return &Keyring{aead: aead}, nil
}

func loadOrCreate(path, envValue string) ([]byte, error) {
	if envValue != "" {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(envValue))
		if err != nil {
			return nil, fmt.Errorf("keyring: CLOUDROOF_MASTER_KEY is not valid base64: %w", err)
		}
		if len(key) != keyLen {
			return nil, fmt.Errorf("keyring: CLOUDROOF_MASTER_KEY must decode to %d bytes, got %d", keyLen, len(key))
		}
		return key, nil
	}

	raw, err := os.ReadFile(path)
	if err == nil {
		key, decErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if decErr != nil {
			return nil, fmt.Errorf("keyring: master key file %s is corrupt: %w", path, decErr)
		}
		if len(key) != keyLen {
			return nil, fmt.Errorf("keyring: master key file %s holds %d bytes, want %d", path, len(key), keyLen)
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("keyring: read master key: %w", err)
	}

	// First run: generate and persist.
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("keyring: generate master key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("keyring: create key dir: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if err := os.WriteFile(path, []byte(encoded+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("keyring: write master key: %w", err)
	}
	return key, nil
}

// Seal encrypts plaintext, returning a self-contained, storable string.
func (k *Keyring) Seal(plaintext []byte) (string, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("keyring: nonce: %w", err)
	}
	sealed := k.aead.Seal(nonce, nonce, plaintext, nil)
	return prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a blob produced by Seal.
func (k *Keyring) OpenSealed(blob string) ([]byte, error) {
	if !strings.HasPrefix(blob, prefix) {
		return nil, errors.New("keyring: unrecognized sealed-blob format")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(blob, prefix))
	if err != nil {
		return nil, fmt.Errorf("keyring: decode sealed blob: %w", err)
	}
	ns := k.aead.NonceSize()
	if len(raw) < ns {
		return nil, errors.New("keyring: sealed blob truncated")
	}
	plaintext, err := k.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		// Almost always means the master key changed or the DB was moved
		// without it. Say so, rather than surfacing a bare crypto error.
		return nil, errors.New("keyring: cannot decrypt secret — wrong master key, or the database was moved without its key file")
	}
	return plaintext, nil
}
