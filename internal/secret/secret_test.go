package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) Resolver {
	t.Helper()
	kf := filepath.Join(t.TempDir(), "credential.key")
	if err := os.WriteFile(kf, []byte("ssh-ed25519-ish key material"), 0o600); err != nil {
		t.Fatal(err)
	}
	return Resolver{Passphrase: "correct horse", KeyFile: kf}
}

func TestRoundTrip(t *testing.T) {
	r := fixture(t)
	sealed, err := Seal(r, "sk-deepseek-plaintext")
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(sealed, Prefix) {
		t.Errorf("sealed value has no %s prefix: %q", Prefix, sealed)
	}
	if strings.Contains(sealed, "sk-deepseek") {
		t.Errorf("the plaintext is visible in the sealed value: %q", sealed)
	}
	got, err := r.Resolve(sealed)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "sk-deepseek-plaintext" {
		t.Errorf("Resolve = %q, want the original plaintext", got)
	}
}

// The salt is per-value, so the same credential sealed twice must not produce
// the same bytes -- otherwise the ciphertext leaks that two agents share a key.
func TestSealingTwiceProducesDifferentCiphertext(t *testing.T) {
	r := fixture(t)
	a, err := Seal(r, "same-key")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Seal(r, "same-key")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two seals of the same plaintext are identical: the salt is not per-value")
	}
}

// A plain value passes through untouched, so a deployment can migrate one
// variable at a time instead of all at once.
func TestAPlainValueIsUntouched(t *testing.T) {
	r := fixture(t)
	got, err := r.Resolve("sk-still-plaintext")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "sk-still-plaintext" {
		t.Errorf("Resolve = %q, want the value unchanged", got)
	}
}

// BOTH factors are required. Either alone must decrypt nothing -- that is the
// entire reason the key file exists rather than a second environment variable.
func TestEitherFactorAloneFails(t *testing.T) {
	r := fixture(t)
	sealed, err := Seal(r, "sk-secret")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("passphrase alone", func(t *testing.T) {
		other := filepath.Join(t.TempDir(), "attacker.key")
		if err := os.WriteFile(other, []byte("a key file the attacker made up"), 0o600); err != nil {
			t.Fatal(err)
		}
		wrong := Resolver{Passphrase: r.Passphrase, KeyFile: other}
		if got, err := wrong.Resolve(sealed); err == nil {
			t.Errorf("the right passphrase with the wrong key file decrypted to %q", got)
		}
	})

	t.Run("key file alone", func(t *testing.T) {
		wrong := Resolver{Passphrase: "guessed", KeyFile: r.KeyFile}
		if got, err := wrong.Resolve(sealed); err == nil {
			t.Errorf("the right key file with the wrong passphrase decrypted to %q", got)
		}
	})
}

// GCM authenticates: an altered value must fail, not decrypt to garbage that
// then goes to the provider as a key.
func TestATamperedValueIsRejected(t *testing.T) {
	r := fixture(t)
	sealed, err := Seal(r, "sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	b := []byte(sealed)
	// Flip a character well past the salt and nonce, inside the ciphertext.
	i := len(b) - 4
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	if got, err := r.Resolve(string(b)); err == nil {
		t.Errorf("a tampered value decrypted to %q", got)
	}
}

// The failure has to name what to fix. An operator seeing "decryption failed"
// with no passphrase set has nothing to act on.
func TestAMissingPassphraseSaysSo(t *testing.T) {
	r := fixture(t)
	sealed, err := Seal(r, "sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Resolver{KeyFile: r.KeyFile}.Resolve(sealed)
	if err == nil {
		t.Fatal("an enc:// value resolved with no passphrase")
	}
	if !strings.Contains(err.Error(), "GANGLION_KEY_PASSPHRASE") {
		t.Errorf("the error does not name the variable to set: %v", err)
	}
}

func TestAMissingKeyFileSaysSo(t *testing.T) {
	r := fixture(t)
	sealed, err := Seal(r, "sk-secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Resolver{Passphrase: r.Passphrase, KeyFile: "/nonexistent/credential.key"}.Resolve(sealed)
	if err == nil {
		t.Fatal("an enc:// value resolved with no key file")
	}
	if !strings.Contains(err.Error(), "/nonexistent/credential.key") {
		t.Errorf("the error does not name the missing file: %v", err)
	}
}

func TestAMalformedValueIsNotAPanic(t *testing.T) {
	r := fixture(t)
	for _, v := range []string{Prefix, Prefix + "!!!not base64!!!", Prefix + "c2hvcnQ="} {
		if _, err := r.Resolve(v); err == nil {
			t.Errorf("%q resolved without error", v)
		}
	}
}
