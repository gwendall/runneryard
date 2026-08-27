package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

func TestDockerContextIncludesEveryGoPackage(t *testing.T) {
	dockerignore, err := os.ReadFile(filepath.Join("..", "..", ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(dockerignore)
	root := filepath.Join("..", "..")
	module := "github.com/gwendall/runneryard/"
	queue := []string{"cmd/runneryard"}
	seen := map[string]bool{}
	for len(queue) > 0 {
		directory := queue[0]
		queue = queue[1:]
		if seen[directory] {
			continue
		}
		seen[directory] = true
		topLevel := strings.SplitN(directory, "/", 2)[0]
		if !strings.Contains(contents, "!"+topLevel+"/") || !strings.Contains(contents, "!"+topLevel+"/**") {
			t.Fatalf("release context omits internal dependency %s", directory)
		}
		files, err := filepath.Glob(filepath.Join(root, directory, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imported := range parsed.Imports {
				path, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(path, module) {
					queue = append(queue, strings.TrimPrefix(path, module))
				}
			}
		}
	}
}
