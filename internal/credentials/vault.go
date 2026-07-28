package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const keychainService = "free-router"

const keychainCommandTimeout = 8 * time.Second

type record struct {
	Backend string `json:"backend"`
	Secret  string `json:"secret,omitempty"`
}

type fileData struct {
	Credentials map[string]record `json:"credentials"`
}

type Entry struct {
	Provider string `json:"provider"`
	Backend  string `json:"backend"`
}

// Vault stores secrets in macOS Keychain when available and otherwise falls
// back to a user-only local file. The file also records which backend is used.
type Vault struct {
	path           string
	useKeychain    bool
	keychainSet    func(string, string) error
	keychainGet    func(string) (string, error)
	keychainDelete func(string) error
	mu             sync.Mutex
}

func New(path string) *Vault {
	return &Vault{
		path: path, useKeychain: runtime.GOOS == "darwin",
		keychainSet: keychainSet, keychainGet: keychainGet, keychainDelete: keychainDelete,
	}
}

// NewFileOnly is intended for environments where a system credential store is
// unavailable, and for tests which must not modify a user's Keychain.
func NewFileOnly(path string) *Vault {
	return &Vault{
		path: path, keychainSet: keychainSet, keychainGet: keychainGet, keychainDelete: keychainDelete,
	}
}

func (v *Vault) Path() string { return v.path }

func (v *Vault) Get(provider string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	data, err := v.load()
	if err != nil {
		return "", false
	}
	rec, ok := data.Credentials[provider]
	if !ok {
		return "", false
	}
	if rec.Backend == "keychain" {
		secret, err := v.keychainGet(provider)
		return secret, err == nil && secret != ""
	}
	return rec.Secret, rec.Secret != ""
}

// Set returns the backend used: "keychain" or "file".
func (v *Vault) Set(provider, secret string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	provider = strings.TrimSpace(provider)
	secret = strings.TrimSpace(secret)
	if provider == "" || secret == "" {
		return "", errors.New("provider and API key must not be empty")
	}
	data, err := v.load()
	if err != nil {
		return "", err
	}

	rec := record{Backend: "file", Secret: secret}
	if v.useKeychain && v.keychainSet(provider, secret) == nil {
		stored, getErr := v.keychainGet(provider)
		if getErr == nil && stored == secret {
			rec = record{Backend: "keychain"}
		} else {
			// A Keychain write can report success even when the daemon's
			// security context cannot read the item back. Persist the secret in
			// the permission-restricted fallback file instead of recording a
			// credential that cannot configure the Provider at runtime.
			_ = v.keychainDelete(provider)
		}
	}
	data.Credentials[provider] = rec
	if err := v.save(data); err != nil {
		if rec.Backend == "keychain" {
			_ = v.keychainDelete(provider)
		}
		return "", err
	}
	return rec.Backend, nil
}

func (v *Vault) Delete(provider string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	data, err := v.load()
	if err != nil {
		return err
	}
	rec, ok := data.Credentials[provider]
	if !ok {
		return fmt.Errorf("no saved credential for %q", provider)
	}
	if rec.Backend == "keychain" {
		_ = v.keychainDelete(provider)
	}
	delete(data.Credentials, provider)
	return v.save(data)
}

func (v *Vault) List() ([]Entry, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	data, err := v.load()
	if err != nil {
		return nil, err
	}
	result := make([]Entry, 0, len(data.Credentials))
	for provider, rec := range data.Credentials {
		result = append(result, Entry{Provider: provider, Backend: rec.Backend})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Provider < result[j].Provider })
	return result, nil
}

func (v *Vault) load() (fileData, error) {
	data := fileData{Credentials: make(map[string]record)}
	content, err := os.ReadFile(v.path)
	if errors.Is(err, os.ErrNotExist) {
		return data, nil
	}
	if err != nil {
		return data, fmt.Errorf("read credentials: %w", err)
	}
	if err := json.Unmarshal(content, &data); err != nil {
		return data, fmt.Errorf("decode credentials: %w", err)
	}
	if data.Credentials == nil {
		data.Credentials = make(map[string]record)
	}
	return data, nil
}

func (v *Vault) save(data fileData) error {
	dir := filepath.Dir(v.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure credentials directory: %w", err)
	}
	content, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return fmt.Errorf("create credentials file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, v.path); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	return os.Chmod(v.path, 0o600)
}

func keychainSet(provider, secret string) error {
	if _, err := os.Stat("/usr/bin/security"); err != nil {
		return err
	}
	// Leaving -w as the final argument makes security read the password instead
	// of exposing it in the process argument list.
	_, err := runCredentialCommand("/usr/bin/security", []string{"add-generic-password", "-U", "-a", provider, "-s", keychainService, "-w"}, secret+"\n", keychainCommandTimeout)
	return err
}

func keychainGet(provider string) (string, error) {
	output, err := runCredentialCommand("/usr/bin/security", []string{"find-generic-password", "-a", provider, "-s", keychainService, "-w"}, "", keychainCommandTimeout)
	return strings.TrimSpace(string(output)), err
}

func keychainDelete(provider string) error {
	_, err := runCredentialCommand("/usr/bin/security", []string{"delete-generic-password", "-a", provider, "-s", keychainService}, "", keychainCommandTimeout)
	return err
}

func runCredentialCommand(path string, args []string, stdin string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, path, args...)
	if stdin != "" {
		command.Stdin = strings.NewReader(stdin)
	}
	output, err := command.Output()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("credential store command timed out after %s", timeout)
	}
	return output, err
}
