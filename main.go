package main

import (
	_ "embed"
	"fmt"
	"os"
	"strings"

	"github.com/sjzsdu/free-router/cmd"
)

//go:embed VERSION
var buildVersion string

func main() {
	cmd.SetVersion(strings.TrimSpace(buildVersion))
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
