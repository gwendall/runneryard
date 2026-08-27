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
	if !strings.Contains(contents, "ARG NODE_VERSION=22.22.3") ||
		!strings.Contains(contents, "ARG NODE_IMAGE=node:${NODE_VERSION}-bookworm-slim@sha256:") {
		t.Fatal("runtime image must pin the Node 22 bootstrap image by digest")
	}
	if !strings.Contains(contents, "COPY --from=node-runtime /usr/local/ /usr/local/") {
		t.Fatal("runtime image must expose Node to shell steps that run before setup-node")
	}
}

func TestRuntimeImagePrewarmsPinnedNodeToolcache(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(dockerfile)
	for _, required := range []string{
		"ARG NODE_VERSION=22.22.3",
		"ARG TARGETARCH",
		`amd64) toolcache_arch=x64`,
		`arm64) toolcache_arch=arm64`,
		`test "${actual_node_version}" = "${expected_node_version}"`,
		`/opt/hostedtoolcache/node/${NODE_VERSION}/${toolcache_arch}`,
		`/opt/hostedtoolcache/node/${NODE_VERSION}/${toolcache_arch}.complete`,
		`chown -R runner:docker /opt/hostedtoolcache`,
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("runtime image does not prewarm the pinned Node toolcache: missing %q", required)
		}
	}
	if strings.Contains(contents, "curl") && strings.Contains(contents, "node-v${NODE_VERSION}") {
		t.Fatal("toolcache prewarming must reuse the digest-pinned Node stage instead of downloading Node again")
	}
}

func TestReleaseRunsPinnedSetupNodeToolcacheCanaryOffline(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(workflow)
	for _, required := range []string{
		"repository: actions/setup-node",
		"ref: 820762786026740c76f36085b0efc47a31fe5020",
		"persist-credentials: false",
		"bash scripts/verify-runtime-toolcache.sh",
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("release workflow does not enforce the setup-node canary: missing %q", required)
		}
	}

	canary, err := os.ReadFile(filepath.Join("..", "..", "scripts", "verify-runtime-toolcache.sh"))
	if err != nil {
		t.Fatal(err)
	}
	contents = string(canary)
	for _, required := range []string{
		"--network none",
		`$setup_node_directory:/action:ro`,
		`x86_64) toolcache_arch=x64`,
		`aarch64|arm64) toolcache_arch=arm64`,
		`node /action/dist/setup/index.js`,
		`grep -Fx "$expected_path" "$GITHUB_PATH"`,
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("runtime toolcache canary is incomplete: missing %q", required)
		}
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
