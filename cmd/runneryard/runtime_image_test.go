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

func TestRuntimeImageIncludesBuildKitTooling(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(dockerfile)
	for _, required := range []string{
		"ARG GO_IMAGE=golang:1.25.3-bookworm@sha256:4f43b271f9673eb7bd0cb3a49cc17b08d8d6ee110277e26dbacc93c43a5a7793",
		"FROM ubuntu:24.04@sha256:33ceb71981b602c1a7443a53469e4dba065f7503eab3078a2d7a57a2ab987517",
		"docker-buildx=0.30.1-0ubuntu1~24.04.1",
		"docker.io=29.1.3-0ubuntu3~24.04.2",
		"ENV DOCKER_BUILDKIT=1",
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("runtime image does not guarantee BuildKit tooling: missing %q", required)
		}
	}
	entrypoint, err := os.ReadFile(filepath.Join("..", "..", "runner-entrypoint"))
	if err != nil {
		t.Fatal(err)
	}
	contents = string(entrypoint)
	for _, required := range []string{
		`{"features":{"buildkit":true}}`,
		"DOCKER_BUILDKIT=1",
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("worker entrypoint does not enable BuildKit: missing %q", required)
		}
	}
	if strings.Contains(contents, `{"storage-driver":"vfs"}`) {
		t.Fatal("worker entrypoint must not force Docker's deep-copy vfs storage driver")
	}
}

func TestRunnerEntrypointPreservesRuntimeTooling(t *testing.T) {
	entrypoint, err := os.ReadFile(filepath.Join("..", "..", "runner-entrypoint"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(entrypoint)
	for _, required := range []string{
		"RUNNER_TOOL_CACHE=/opt/hostedtoolcache ImageOS=ubuntu24",
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("worker entrypoint does not preserve the toolcache contract: missing %q", required)
		}
	}
}

func TestReleaseBuildsAndCanariesBothWorkerArchitectures(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(workflow)
	for _, required := range []string{
		"docker/setup-qemu-action@c7c53464625b32c7a7e944ae62b3e17d2b600130",
		"docker/setup-buildx-action@8d2750c68a42422c14e847fe6c8ac0403b4cbd6f",
		"linux/amd64:amd64 linux/arm64:arm64",
		`"$image:$version-$arch"`,
		`verify-runtime-toolcache.sh`,
		`"$node_version" "$platform"`,
		`docker buildx imagetools create`,
		`sort == ["amd64", "arm64"]`,
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("release does not publish and canary both worker architectures: missing %q", required)
		}
	}
}

func TestRuntimeImageIncludesPinnedNodeBootstrap(t *testing.T) {
	dockerfile, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(dockerfile)
	if !strings.Contains(contents, "ARG NODE_VERSION="+runtimeNodeVersion) ||
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
		"ARG NODE_VERSION=" + runtimeNodeVersion,
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
		`node_version="$(sed -n 's/^ARG NODE_VERSION=//p' Dockerfile)"`,
		`Dockerfile does not declare ARG NODE_VERSION`,
		"bash scripts/verify-runtime-toolcache.sh",
		`"$architecture_image" .canary/setup-node "$node_version" "$platform"`,
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
		`docker_platform_args=(--platform "$platform")`,
		`--entrypoint docker`,
		`"$runtime_image" buildx version`,
		`--entrypoint /usr/local/bin/runner-entrypoint`,
		`ACTIONS_RUNNER_INPUT_JITCONFIG=offline-canary`,
		`RUNNERYARD_DEADLINE=2099-01-01T00:00:00Z`,
		`test "$RUNNER_TOOL_CACHE" = /opt/hostedtoolcache`,
		`grep -Fx passed "$entrypoint_canary_directory/output/result"`,
		`workflow_commands_token="runneryard-$(openssl rand -hex 32)"`,
		`echo "::stop-commands::$workflow_commands_token"`,
		`$setup_node_directory:/action:ro`,
		`x86_64) toolcache_arch=x64`,
		`aarch64|arm64) toolcache_arch=arm64`,
		`node /action/dist/setup/index.js`,
		`grep -Fx "$expected_path" "$GITHUB_PATH"`,
		`echo "::$workflow_commands_token::"`,
		`exit "$canary_status"`,
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("runtime toolcache canary is incomplete: missing %q", required)
		}
	}
}

func TestReleasePublishesImmutableArtifactsFailClosed(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(workflow)
	for _, required := range []string{
		`gh api --paginate`,
		`Organization) package_owner_path="orgs/${GITHUB_REPOSITORY_OWNER}"`,
		`grep -Fxq "$release_tag" "$existing_tags"`,
		`Refusing to overwrite immutable GHCR tag`,
		`draft: true`,
		`gh release edit "$VERSION" --draft=false --verify-tag`,
		`--jq .immutable`,
	} {
		if !strings.Contains(contents, required) {
			t.Fatalf("release workflow does not fail closed on immutable artifacts: missing %q", required)
		}
	}

	draft := strings.Index(contents, "draft: true")
	publish := strings.Index(contents, `gh release edit "$VERSION" --draft=false --verify-tag`)
	if draft == -1 || publish == -1 || draft >= publish {
		t.Fatal("release workflow must attach every asset to a draft before publishing it")
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
