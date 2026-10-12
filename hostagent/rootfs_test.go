package hostagent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// flyRootMounts is the start of /proc/self/mounts on a Fly Machine with a
// volume, read on 2026-10-12: the image is vda, Fly's kernel modules vdd.
const flyRootMounts = `devtmpfs /dev devtmpfs rw,nosuid,relatime,size=477380k,nr_inodes=119345,mode=755 0 0
none / overlay rw,relatime,lowerdir=/lower/dev/vda:/lower/dev/vdd,upperdir=/upper/upper,workdir=/upper/workdir,uuid=on 0 0
/dev/vdb /.fly-upper-layer ext4 rw,noatime,nobarrier,nombcache 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
/dev/vdc /data ext4 rw,relatime 0 0
`

func TestLocalImageName(t *testing.T) {
	t.Parallel()
	for image, want := range map[string]string{
		"ghcr.io/acme/runner:0.5.0":                    "ghcr.io/acme/runner:0.5.0",
		"ghcr.io/acme/runner@sha256:2222":              "ghcr.io/acme/runner:sha256-2222",
		"registry.fly.io/workers:v3@sha256:abcd":       "registry.fly.io/workers:sha256-abcd",
		"localhost:5000/runner@sha256:abcd":            "localhost:5000/runner:sha256-abcd",
		"localhost:5000/runner:edge@sha256:abcd":       "localhost:5000/runner:sha256-abcd",
		"runner@sha256:abcd":                           "runner:sha256-abcd",
		"registry.fly.io/workers:deployment-01JABCDEF": "registry.fly.io/workers:deployment-01JABCDEF",
	} {
		if got := LocalImageName(image); got != want {
			t.Errorf("LocalImageName(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestImageLayerIsTheTopLowerLayerOfTheRootOverlay(t *testing.T) {
	t.Parallel()
	withoutVolume := strings.Replace(flyRootMounts, "/lower/dev/vdd", "/lower/dev/vdc", 1)
	for name, mounts := range map[string]string{"with a volume": flyRootMounts, "without a volume": withoutVolume} {
		if device, err := ImageLayer(mounts); err != nil || device != "/dev/vda" {
			t.Errorf("%s: ImageLayer = %q, %v; want /dev/vda", name, device, err)
		}
	}
	for name, mounts := range map[string]string{
		"plain root":          "/dev/sda1 / ext4 rw,relatime 0 0\n",
		"overlay on a folder": "overlay / overlay rw,lowerdir=/var/lib/image,upperdir=/u,workdir=/w 0 0\n",
		"escaped name":        "none / overlay rw,lowerdir=/lower/dev/vd\\040a,upperdir=/u,workdir=/w 0 0\n",
		"partition path":      "none / overlay rw,lowerdir=/lower/dev/disk/by-id/x,upperdir=/u,workdir=/w 0 0\n",
		"no root":             "proc /proc proc rw 0 0\n",
	} {
		if device, err := ImageLayer(mounts); err == nil {
			t.Errorf("%s: ImageLayer = %q, want an error", name, device)
		}
	}
}

func TestImageEnvironmentKeepsOnlyWhatTheImageSets(t *testing.T) {
	t.Parallel()
	// The boot process's environment on a Fly host from the release image.
	environ := []string{
		"DEBIAN_FRONTEND=noninteractive",
		"DOCKER_BUILDKIT=1",
		"FLY_ALLOC_ID=894921f66341e8",
		"FLY_APP_NAME=ry-probe",
		"FLY_IMAGE_REF=ghcr.io/gwendall/runneryard:0.5.0",
		"FLY_PRIVATE_IP=fdaa:0:1::2",
		"HOME=/home/runner",
		"ImageOS=ubuntu24",
		"PATH=/opt/x/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"RUNNERYARD_DOCKER_DNS=1.1.1.1",
		"RUNNER_TOOL_CACHE=/opt/hostedtoolcache",
		"TERM=linux",
		"cgroup_enable=memory",
		"npm_config_store_dir=/opt/pnpm-store",
		`QUOTED=say "hi" $HOME \n`,
	}
	initEnviron := []string{"HOME=/", "TERM=linux", "cgroup_enable=memory", ""}
	env, err := ImageEnvironment(environ, initEnviron)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"DEBIAN_FRONTEND=noninteractive",
		"DOCKER_BUILDKIT=1",
		"HOME=/home/runner",
		"ImageOS=ubuntu24",
		"PATH=/opt/x/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"RUNNER_TOOL_CACHE=/opt/hostedtoolcache",
		"npm_config_store_dir=/opt/pnpm-store",
		`QUOTED=say "hi" $HOME \n`,
	}
	if !slices.Equal(env, want) {
		t.Fatalf("image environment =\n%q\nwant\n%q", env, want)
	}

	for _, entry := range []string{"MULTI=line\nbreak", "BELL=\a", "bad key=1", "=empty"} {
		if env, err := ImageEnvironment([]string{entry}, nil); err == nil && len(env) > 0 {
			t.Errorf("ImageEnvironment(%q) = %q, want an error", entry, env)
		}
	}
	if env, err := ImageEnvironment([]string{"TABBED=a\tb"}, nil); err != nil || len(env) != 1 {
		t.Errorf("a tab is a valid value: %q, %v", env, err)
	}
}

func TestImportChangesQuoteEveryValue(t *testing.T) {
	t.Parallel()
	got := importChanges([]string{"A=plain", `B=say "hi"`, `C=$HOME and ${X}`, `D=back\slash\`, "E=", "F=x=y"})
	// The same instructions, given to docker import, read back as the exact
	// values (checked against Docker 29.5 on 2026-10-12).
	want := []string{
		"USER runner",
		`ENV A="plain"`,
		`ENV B="say \"hi\""`,
		`ENV C="\$HOME and \${X}"`,
		`ENV D="back\\slash\\"`,
		`ENV E=""`,
		`ENV F="x=y"`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("changes =\n%q\nwant\n%q", got, want)
	}
}

func TestImportRootfsImageNeedsTheImageLayer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mounts")
	if err := os.WriteFile(path, []byte("/dev/sda1 / ext4 rw,relatime 0 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := mountsPath
	mountsPath = path
	t.Cleanup(func() { mountsPath = previous })
	if err := ImportRootfsImage(context.Background(), "runner:local"); err == nil || !strings.Contains(err.Error(), "not an overlay") {
		t.Fatalf("ImportRootfsImage on a plain root = %v, want the overlay error before any mount", err)
	}
}

// fakeTools puts a tar and a docker on PATH that record what they did.
func fakeTools(t *testing.T, tarStatus string) string {
	t.Helper()
	dir := t.TempDir()
	tools := map[string]string{
		"tar": `#!/bin/sh
printf 'layer-bytes'
exit ` + tarStatus + `
`,
		"docker": `#!/bin/sh
log="$(dirname "$0")/docker.log"
case "$1" in
import) cat > "$(dirname "$0")/imported"; echo "$*" >> "$log"; echo sha256:feed ;;
*) echo "$*" >> "$log" ;;
esac
`,
	}
	for name, script := range tools {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil { // #nosec G306 -- test tool
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestImportTreeReturnsTheImageOfACompleteTree(t *testing.T) {
	dir := fakeTools(t, "0")
	id, err := importTree(context.Background(), t.TempDir(), []string{"USER runner", `ENV A="1"`})
	if err != nil || id != "sha256:feed" {
		t.Fatalf("importTree = %q, %v; want the imported image ID", id, err)
	}
	imported, err := os.ReadFile(filepath.Join(dir, "imported")) // #nosec G304 -- test file
	if err != nil || string(imported) != "layer-bytes" {
		t.Fatalf("docker import read %q, %v; want the tar stream", imported, err)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "docker.log")) // #nosec G304 -- test file
	if got := strings.TrimSpace(string(log)); got != `import --change USER runner --change ENV A="1" -` {
		t.Fatalf("docker calls = %q, want one untagged import", got)
	}
}

func TestImportTreeRemovesTheImageOfAFailedTar(t *testing.T) {
	dir := fakeTools(t, "2")
	if id, err := importTree(context.Background(), t.TempDir(), []string{"USER runner"}); err == nil {
		t.Fatalf("importTree = %q after tar failed, want an error", id)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "docker.log")) // #nosec G304 -- test file
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if want := []string{"import --change USER runner -", "image rm sha256:feed"}; !slices.Equal(lines, want) {
		t.Fatalf("docker calls = %q, want the partial image removed and never tagged", lines)
	}
}
