package main

import (
	"fmt"
	"os"

	"github.com/sjzsdu/free-router/cmd"
)

// buildVersion is injected by the build pipeline from the nearest git tag:
//
//	go build -ldflags "-X main.buildVersion=$(git describe --tags --always --dirty | sed 's/^v//')" .
var buildVersion = "dev"

func main() {
	cmd.SetVersion(buildVersion)
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
