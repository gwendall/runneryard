package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRuntimeImageIncludesGitHubCLI(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)apt-get install[^\n]*\\\n(?:.*\\\n)*?\s+.*\bgh\b`).Match(dockerfile) {
		t.Fatal("runtime image must include gh for migrated monorepo gates")
	}
}

func TestRuntimeImageIncludesPinnedNodeBootstrap(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(dockerfile)
	if !strings.Contains(contents, "ARG NODE_IMAGE=node:22.22.3-bookworm-slim@sha256:") {
		t.Fatal("runtime image must pin the Node 22 bootstrap image by digest")
	}
	if !strings.Contains(contents, "COPY --from=node-runtime /usr/local/ /usr/local/") {
		t.Fatal("runtime image must expose Node to shell steps that run before setup-node")
	}
}
