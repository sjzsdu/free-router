package cmd

import "testing"

func TestAdminConsoleURL(t *testing.T) {
	for input, expected := range map[string]string{
		":1314":                  "http://localhost:1314/admin/",
		"127.0.0.1:8080":         "http://localhost:8080/admin/",
		"http://0.0.0.0:9191":    "http://localhost:9191/admin/",
		"invalid-listen-address": "http://localhost:1314/admin/",
	} {
		if actual := adminConsoleURL(input); actual != expected {
			t.Errorf("adminConsoleURL(%q) = %q, want %q", input, actual, expected)
		}
	}
}
