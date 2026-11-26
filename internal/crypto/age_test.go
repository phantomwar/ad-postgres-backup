package crypto

import (
	"os"
	"path/filepath"
	"testing"
)

func TestX25519RoundtripAndTamperChecks(t *testing.T) {
	pub, sec, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	plainPath := filepath.Join(tmp, "dump.sql.gz")
	encPath := filepath.Join(tmp, "dump.sql.gz.age")
	decPath := filepath.Join(tmp, "restored.sql.gz")

	payload := []byte("CREATE TABLE t (id int); INSERT INTO t VALUES (42);\n")
	if err := os.WriteFile(plainPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := EncryptFile(plainPath, encPath, EncryptionConfig{Recipients: []string{pub}}); err != nil {
		t.Fatalf("EncryptFile: %v", err)
	}

	sumPath, err := WriteSHA256File(encPath)
	if err != nil {
		t.Fatalf("WriteSHA256File: %v", err)
	}
	if err := VerifySHA256File(encPath); err != nil {
		t.Fatalf("VerifySHA256File: %v", err)
	}

	// Wrong key must fail
	_, wrongSec, err := GenerateKeypair()
	if err != nil {
		t.Fatal(err)
	}
	if err := DecryptFile(encPath, decPath, DecryptionConfig{Identity: wrongSec}); err == nil {
		t.Fatal("expected wrong identity to fail decryption")
	}

	// Valid key succeeds
	if err := DecryptFile(encPath, decPath, DecryptionConfig{Identity: sec}); err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	got, err := os.ReadFile(decPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("roundtrip mismatch: got %q, want %q", got, payload)
	}

	// Tampering with encrypted file fails SHA256 verification
	encBytes, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatal(err)
	}
	encBytes[len(encBytes)-1] ^= 0xFF
	if err := os.WriteFile(encPath, encBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySHA256File(encPath); err == nil {
		t.Fatalf("expected SHA256 mismatch after tampering (%s)", sumPath)
	}
	if err := DecryptFile(encPath, decPath, DecryptionConfig{Identity: sec}); err == nil {
		t.Fatal("expected DecryptFile to fail on truncated/corrupted ciphertext")
	}
}

func TestPassphraseRoundtrip(t *testing.T) {
	tmp := t.TempDir()
	plainPath := filepath.Join(tmp, "data.txt")
	encPath := filepath.Join(tmp, "data.txt.age")
	decPath := filepath.Join(tmp, "data.out")

	if err := os.WriteFile(plainPath, []byte("hello passphrase"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EncryptFile(plainPath, encPath, EncryptionConfig{Passphrase: "correct-horse-battery"}); err != nil {
		t.Fatal(err)
	}
	if err := DecryptFile(encPath, decPath, DecryptionConfig{Passphrase: "wrong-pass"}); err == nil {
		t.Fatal("expected wrong passphrase to fail")
	}
	if err := DecryptFile(encPath, decPath, DecryptionConfig{Passphrase: "correct-horse-battery"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(decPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello passphrase" {
		t.Fatalf("got %q", got)
	}
}
