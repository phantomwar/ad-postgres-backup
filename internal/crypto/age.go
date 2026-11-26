package crypto

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
)

// EncryptionConfig selects either asymmetric age recipients or a symmetric passphrase.
type EncryptionConfig struct {
	Recipients []string
	Passphrase string
}

// DecryptionConfig selects either an age identity block or a symmetric passphrase.
type DecryptionConfig struct {
	Identity   string
	Passphrase string
}

// GenerateKeypair returns a fresh X25519 age keypair (public recipient, secret identity).
func GenerateKeypair() (string, string, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", fmt.Errorf("generate X25519 identity: %w", err)
	}
	return id.Recipient().String(), id.String(), nil
}

// EncryptFile streams srcPath into dstPath atomically using age encryption.
func EncryptFile(srcPath, dstPath string, cfg EncryptionConfig) error {
	recipients, err := buildRecipients(cfg)
	if err != nil {
		return err
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open plaintext %q: %w", srcPath, err)
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(dstPath), 0o750); err != nil {
		return err
	}
	tmpPath := dstPath + ".tmp"
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create temp encrypted file: %w", err)
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	encWriter, err := age.Encrypt(dst, recipients...)
	if err != nil {
		_ = dst.Close()
		return fmt.Errorf("initialize age encryption: %w", err)
	}
	if _, err := io.Copy(encWriter, src); err != nil {
		_ = encWriter.Close()
		_ = dst.Close()
		return fmt.Errorf("stream encrypt: %w", err)
	}
	if err := encWriter.Close(); err != nil {
		_ = dst.Close()
		return fmt.Errorf("finalize age encryption: %w", err)
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return fmt.Errorf("sync encrypted file: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close encrypted file: %w", err)
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		return fmt.Errorf("rename %q -> %q: %w", tmpPath, dstPath, err)
	}
	cleanup = false
	return nil
}

// DecryptFile streams an age-encrypted srcPath into dstPath atomically.
func DecryptFile(srcPath, dstPath string, cfg DecryptionConfig) error {
	identities, err := buildIdentities(cfg)
	if err != nil {
		return err
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open encrypted file %q: %w", srcPath, err)
	}
	defer src.Close()

	decReader, err := age.Decrypt(src, identities...)
	if err != nil {
		return fmt.Errorf("age decrypt header: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dstPath), 0o750); err != nil {
		return err
	}
	tmpPath := dstPath + ".tmp"
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create temp decrypted file: %w", err)
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(dst, decReader); err != nil {
		_ = dst.Close()
		return fmt.Errorf("stream decrypt: %w", err)
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		return fmt.Errorf("sync decrypted file: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("close decrypted file: %w", err)
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		return fmt.Errorf("rename %q -> %q: %w", tmpPath, dstPath, err)
	}
	cleanup = false
	return nil
}

// WriteSHA256File computes the streaming SHA-256 digest of filePath and writes `<hex>  <basename>` to filePath+".sha256".
func WriteSHA256File(filePath string) (string, error) {
	sum, err := FileSHA256(filePath)
	if err != nil {
		return "", err
	}
	line := fmt.Sprintf("%s  %s\n", sum, filepath.Base(filePath))
	sumPath := filePath + ".sha256"
	if err := os.WriteFile(sumPath, []byte(line), 0o600); err != nil {
		return "", fmt.Errorf("write sha256 file: %w", err)
	}
	return sumPath, nil
}

// VerifySHA256File verifies filePath against filePath+".sha256". If no sidecar exists, returns nil.
func VerifySHA256File(filePath string) error {
	sumPath := filePath + ".sha256"
	data, err := os.ReadFile(sumPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read sha256 file: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return fmt.Errorf("empty sha256 sidecar %q", sumPath)
	}
	expected := strings.ToLower(fields[0])
	actual, err := FileSHA256(filePath)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("sha256 mismatch for %q: got %s, want %s", filePath, actual, expected)
	}
	return nil
}

// FileSHA256 streams filePath and returns its lowercase hex SHA-256 digest.
func FileSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open %q for sha256: %w", filePath, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %q: %w", filePath, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func buildRecipients(cfg EncryptionConfig) ([]age.Recipient, error) {
	if len(cfg.Recipients) > 0 && cfg.Passphrase != "" {
		return nil, fmt.Errorf("ambiguous encryption config: both recipients and passphrase specified")
	}
	if len(cfg.Recipients) > 0 {
		recs, err := age.ParseRecipients(strings.NewReader(strings.Join(cfg.Recipients, "\n")))
		if err != nil {
			return nil, fmt.Errorf("parse age recipients: %w", err)
		}
		return recs, nil
	}
	if cfg.Passphrase != "" {
		r, err := age.NewScryptRecipient(cfg.Passphrase)
		if err != nil {
			return nil, fmt.Errorf("create scrypt recipient: %w", err)
		}
		return []age.Recipient{r}, nil
	}
	return nil, fmt.Errorf("no encryption recipient or passphrase configured")
}

func buildIdentities(cfg DecryptionConfig) ([]age.Identity, error) {
	if cfg.Identity != "" {
		ids, err := age.ParseIdentities(strings.NewReader(cfg.Identity))
		if err != nil {
			return nil, fmt.Errorf("parse age identity: %w", err)
		}
		return ids, nil
	}
	if cfg.Passphrase != "" {
		id, err := age.NewScryptIdentity(cfg.Passphrase)
		if err != nil {
			return nil, fmt.Errorf("create scrypt identity: %w", err)
		}
		return []age.Identity{id}, nil
	}
	return nil, fmt.Errorf("no age identity or passphrase configured for decryption")
}
