// Package secret resolves credentials written as enc:// URIs.
//
// # WHAT THIS PROTECTS, AND WHAT IT DOES NOT
//
// It protects the credential AT REST: in dokploy's environment editor, in
// deploy/*/.env, in `docker inspect`, in a backup of any of them. Those are
// the places a provider key currently sits in plaintext, and each of them is
// read by more people and more tools than the harness process is.
//
// It does NOT hide the key from the running harness -- it cannot. The process
// has to present the key to the provider, so it holds plaintext in memory for
// as long as it runs. Anyone who tells you otherwise about any scheme of this
// shape is wrong. What keeps the key from the AGENT is the other two controls:
// the environment scrub, and the Landlock domain that denies /proc.
//
// # TWO FACTORS, AND WHY IT IS POINTLESS WITH ONE
//
// A passphrase in the environment next to the ciphertext in the environment is
// ceremony: one `docker inspect` yields both. So the key is derived from a
// passphrase (environment) AND a key file (a read-only bind mounted outside
// the agent's workspace and outside the Landlock ruleset). Reading the
// environment is then not enough, and neither is reading the volume.
//
// This mirrors picoclaw's scheme -- SHA256 of a private key file, HMAC'd with
// a passphrase, HKDF'd to an AES key -- deliberately, so the two harnesses do
// not need two different mental models. See
// https://docs.picoclaw.io/docs/credential-encryption/
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Prefix marks a value as encrypted. A value without it is used as-is, so a
// deployment migrates one variable at a time.
const Prefix = "enc://"

const (
	saltLen  = 16
	nonceLen = 12

	// info binds the derived key to this purpose and this version. Changing
	// the scheme means changing this string, which makes old ciphertext fail
	// to decrypt loudly instead of producing garbage.
	info = "ganglion-credential-v1"
)

// Resolver holds the two factors.
type Resolver struct {
	Passphrase string
	KeyFile    string
}

// IsEncrypted reports whether a value needs this package at all.
func IsEncrypted(v string) bool { return strings.HasPrefix(v, Prefix) }

// Resolve returns v unchanged when it is not an enc:// URI, and the plaintext
// when it is.
func (r Resolver) Resolve(v string) (string, error) {
	if !IsEncrypted(v) {
		return v, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, Prefix))
	if err != nil {
		return "", fmt.Errorf("enc:// value is not valid base64: %w", err)
	}
	if len(raw) < saltLen+nonceLen {
		return "", errors.New("enc:// value is truncated")
	}
	salt, nonce, ct := raw[:saltLen], raw[saltLen:saltLen+nonceLen], raw[saltLen+nonceLen:]

	gcm, err := r.aead(salt)
	if err != nil {
		return "", err
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// GCM authenticates, so this is "wrong key or tampered value" and
		// cannot distinguish the two. Say both, because the operator's next
		// move differs: a wrong passphrase is a typo, a wrong key file is a
		// missing bind.
		return "", errors.New("enc:// value did not decrypt: wrong passphrase, wrong key file, or the value was altered")
	}
	return string(pt), nil
}

// Seal encrypts a plaintext into an enc:// URI. Used by the encrypt
// subcommand; the harness itself only ever decrypts.
func Seal(r Resolver, plaintext string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	gcm, err := r.aead(salt)
	if err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return Prefix + base64.StdEncoding.EncodeToString(append(append(salt, nonce...), ct...)), nil
}

// aead derives the AES-256-GCM key for one salt.
//
//	fileHash = SHA256(key file)
//	ikm      = HMAC-SHA256(key: fileHash, message: passphrase)
//	aesKey   = HKDF-SHA256(ikm, salt, info, 32)
//
// The salt is per-value and stored with the ciphertext, so encrypting the same
// key twice produces different bytes and neither reveals that they match.
func (r Resolver) aead(salt []byte) (cipher.AEAD, error) {
	if r.Passphrase == "" {
		return nil, errors.New("GANGLION_KEY_PASSPHRASE is unset, and an enc:// value cannot be resolved without it")
	}
	keyBytes, err := os.ReadFile(r.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read the credential key file %s: %w "+
			"(it is a read-only bind, outside the workspace on purpose)", r.KeyFile, err)
	}
	if len(keyBytes) == 0 {
		return nil, fmt.Errorf("the credential key file %s is empty", r.KeyFile)
	}

	fileHash := sha256.Sum256(keyBytes)
	mac := hmac.New(sha256.New, fileHash[:])
	mac.Write([]byte(r.Passphrase))
	ikm := mac.Sum(nil)

	aesKey, err := hkdf.Key(sha256.New, ikm, salt, info, 32)
	if err != nil {
		return nil, fmt.Errorf("derive the credential key: %w", err)
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
