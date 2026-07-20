package appdirs

import (
	"os"
	"path/filepath"
)

const directoryName = ".free-router"

// ForHome returns the single directory used for free-router configuration and data.
func ForHome(home string) string {
	return filepath.Join(home, directoryName)
}

// Default returns the current user's free-router configuration and data directory.
func Default() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), directoryName)
	}
	return ForHome(home)
}
