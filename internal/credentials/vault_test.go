package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileVaultLifecycleAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "credentials.json")
	vault := NewFileOnly(path)

	backend, err := vault.Set("groq", "saved-secret")
	if err != nil {
		t.Fatal(err)
	}
	if backend != "file" {
		t.Fatalf("backend = %q", backend)
	}
	secret, ok := vault.Get("groq")
	if !ok || secret != "saved-secret" {
		t.Fatalf("Get() = %q, %v", secret, ok)
	}
	entries, err := vault.List()
	if err != nil || len(entries) != 1 || entries[0].Provider != "groq" || entries[0].Backend != "file" {
		t.Fatalf("List() = %#v, %v", entries, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %o", info.Mode().Perm())
	}
	if err := vault.Delete("groq"); err != nil {
		t.Fatal(err)
	}
	if _, ok := vault.Get("groq"); ok {
		t.Fatal("credential still exists after delete")
	}
}

func TestVaultFallsBackToFileWhenKeychainCannotReadWrittenSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	deleted := false
	vault := &Vault{
		path:        path,
		useKeychain: true,
		keychainSet: func(_, _ string) error { return nil },
		keychainGet: func(string) (string, error) {
			return "", errors.New("keychain item is not readable by this process")
		},
		keychainDelete: func(string) error {
			deleted = true
			return nil
		},
	}

	backend, err := vault.Set("dashscope", "saved-secret")
	if err != nil {
		t.Fatal(err)
	}
	if backend != "file" || !deleted {
		t.Fatalf("backend=%q deleted=%t", backend, deleted)
	}
	secret, ok := vault.Get("dashscope")
	if !ok || secret != "saved-secret" {
		t.Fatalf("Get()=%q, %t", secret, ok)
	}
	entries, err := vault.List()
	if err != nil || len(entries) != 1 || entries[0].Backend != "file" {
		t.Fatalf("List()=%#v, %v", entries, err)
	}
}

func TestVaultUsesKeychainOnlyAfterSuccessfulReadback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	stored := ""
	vault := &Vault{
		path:        path,
		useKeychain: true,
		keychainSet: func(_, secret string) error {
			stored = secret
			return nil
		},
		keychainGet:    func(string) (string, error) { return stored, nil },
		keychainDelete: func(string) error { return nil },
	}

	backend, err := vault.Set("groq", "saved-secret")
	if err != nil {
		t.Fatal(err)
	}
	if backend != "keychain" {
		t.Fatalf("backend=%q", backend)
	}
	secret, ok := vault.Get("groq")
	if !ok || secret != "saved-secret" {
		t.Fatalf("Get()=%q, %t", secret, ok)
	}
}

func TestCredentialCommandTimesOut(t *testing.T) {
	started := time.Now()
	_, err := runCredentialCommand("/bin/sleep", []string{"2"}, "", 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}
