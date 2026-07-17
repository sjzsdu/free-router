package credentials

import (
	"os"
	"path/filepath"
	"testing"
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
