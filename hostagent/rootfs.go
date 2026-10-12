package hostagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A machine that boots from the runner image, such as a Fly host, already
// holds that image: it is the machine's root filesystem. Importing it into
// the host's Docker gives the slots the image without pulling it a second
// time, and without a registry credential on the host, which a private image
// would otherwise need.

const (
	// BootDNSVariable carries the slot resolvers from the machine
	// configuration to host boot.
	BootDNSVariable = "RUNNERYARD_DOCKER_DNS"
	// slotEntrypoint is what a slot runs; an image without it is not a runner
	// image.
	slotEntrypoint      = "/usr/local/bin/runner-entrypoint"
	rootfsImportTimeout = 15 * time.Minute
)

// mountsPath and initEnvironPath are read by ImportRootfsImage; tests point
// them at files.
var (
	mountsPath      = "/proc/self/mounts"
	initEnvironPath = "/proc/1/environ"
)

var (
	blockDeviceName = regexp.MustCompile(`^/dev/[a-z][a-z0-9]{0,31}$`)
	envKey          = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,254}$`)
	// dockerfileQuoted escapes a value inside double quotes of a Dockerfile
	// ENV instruction, where a backslash keeps only these three characters.
	dockerfileQuoted = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`)
)

// LocalImageName is the name the host's Docker gives the runner image. Docker
// cannot tag an image with a digest, so an image pinned by digest is tagged
// with it instead: registry/runner@sha256:abc becomes
// registry/runner:sha256-abc.
func LocalImageName(image string) string {
	name, digest, pinned := strings.Cut(image, "@")
	if !pinned {
		return image
	}
	last := strings.LastIndex(name, "/") + 1
	if colon := strings.Index(name[last:], ":"); colon >= 0 {
		name = name[:last+colon]
	}
	return name + ":" + strings.ReplaceAll(digest, ":", "-")
}

// ImageLayer returns the block device that holds the machine's image, from
// the contents of /proc/self/mounts. Fly mounts the image's filesystem
// read-only, from /lower/dev/<device>, as the topmost lower layer of the
// overlay at /; what it writes at boot (hosts, resolv.conf, the machine's
// files and keys) lives in the upper layer.
func ImageLayer(mounts string) (string, error) {
	lower := ""
	for line := range strings.Lines(mounts) {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "/" || fields[2] != "overlay" {
			continue
		}
		lower = ""
		for option := range strings.SplitSeq(fields[3], ",") {
			if value, ok := strings.CutPrefix(option, "lowerdir="); ok {
				lower, _, _ = strings.Cut(value, ":")
			}
		}
	}
	at := strings.LastIndex(lower, "/dev/")
	if at < 0 {
		return "", errors.New("the root filesystem is not an overlay over a block device")
	}
	if device := lower[at:]; blockDeviceName.MatchString(device) {
		return device, nil
	}
	return "", fmt.Errorf("unexpected image layer %q", lower)
}

// ImageEnvironment returns the environment the image sets, from the boot
// process's own. Fly gives the process the image's environment, adds its
// FLY_ variables and the machine configuration's (BootDNSVariable), and its
// init passes on its own environment (TERM, kernel parameters), which counts
// only where the image set another value. A variable that a Dockerfile ENV
// instruction cannot carry fails the import rather than be dropped.
func ImageEnvironment(environ, initEnviron []string) ([]string, error) {
	inherited := make(map[string]bool, len(initEnviron))
	for _, entry := range initEnviron {
		inherited[entry] = true
	}
	var env []string
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || inherited[entry] || strings.HasPrefix(key, "FLY_") || key == BootDNSVariable {
			continue
		}
		if !envKey.MatchString(key) || strings.ContainsFunc(value, func(r rune) bool { return r < ' ' && r != '\t' || r == 0x7f }) {
			return nil, fmt.Errorf("environment variable %q cannot be set in an image", key)
		}
		env = append(env, entry)
	}
	return env, nil
}

// importChanges are the docker import instructions that give the imported
// filesystem the image's configuration. Fly reports neither the image's user
// nor its configuration to the machine; a runner image ends with USER runner,
// as the release image does and the derived image guide requires. A slot sets
// its own entrypoint and command.
func importChanges(env []string) []string {
	changes := []string{"USER runner"}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		changes = append(changes, "ENV "+key+`="`+dockerfileQuoted.Replace(value)+`"`)
	}
	return changes
}

// ImportRootfsImage imports the machine's image layer into the host's Docker
// as image. It reads a read-only mount of the layer's block device, so
// nothing Fly wrote at boot, and none of the host's keys, enters the image.
// The image is tagged only once the whole filesystem was read.
func ImportRootfsImage(ctx context.Context, image string) error {
	mounts, err := os.ReadFile(mountsPath)
	if err != nil {
		return err
	}
	device, err := ImageLayer(string(mounts))
	if err != nil {
		return err
	}
	if info, err := os.Stat(device); err != nil {
		return err
	} else if info.Mode()&os.ModeDevice == 0 || info.Mode()&os.ModeCharDevice != 0 {
		return fmt.Errorf("%s is not a block device", device)
	}
	initEnviron, err := os.ReadFile(initEnvironPath)
	if err != nil {
		return err
	}
	env, err := ImageEnvironment(os.Environ(), strings.Split(string(initEnviron), "\x00"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, rootfsImportTimeout)
	defer cancel()
	dir, err := os.MkdirTemp("", "runneryard-image-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(dir) }()
	if err := run(ctx, "mount", "-o", "ro,nosuid,nodev,noexec", device, dir); err != nil {
		return err
	}
	defer func() { _ = run(context.WithoutCancel(ctx), "umount", dir) }()
	if info, err := os.Lstat(filepath.Join(dir, slotEntrypoint)); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s holds no %s: not a runner image", device, slotEntrypoint)
	}
	id, err := importTree(ctx, dir, importChanges(env))
	if err != nil {
		return err
	}
	return run(ctx, "docker", "tag", id, image)
}

// importTree streams the tree at dir into docker import and returns the new
// image's ID. A tar that fails leaves an untagged image at most, which it
// removes.
func importTree(ctx context.Context, dir string, changes []string) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer, err := os.Pipe()
	if err != nil {
		return "", err
	}
	// File capabilities are the only extended attributes a Docker layer keeps.
	archive := exec.CommandContext(ctx, "tar", "--create", "--file=-", "--directory="+dir, "--one-file-system", // #nosec G204 -- fixed tool, mount point we created
		"--numeric-owner", "--xattrs", "--xattrs-include=security.capability", ".")
	archive.Stdout = writer
	archiveErr := &boundedBuffer{limit: maxStderrBytes}
	archive.Stderr = archiveErr
	args := []string{"import"}
	for _, change := range changes {
		args = append(args, "--change", change)
	}
	importer := exec.CommandContext(ctx, "docker", append(args, "-")...) // #nosec G204 -- fixed docker binary, discrete arguments, no shell
	importer.Stdin = reader
	var id strings.Builder
	importer.Stdout = &id
	importErr := &boundedBuffer{limit: maxStderrBytes}
	importer.Stderr = importErr
	startErr := importer.Start()
	if startErr == nil {
		if startErr = archive.Start(); startErr != nil {
			cancel()
		}
	}
	// The children hold their own ends of the pipe.
	_ = reader.Close()
	_ = writer.Close()
	if startErr != nil {
		if importer.Process != nil {
			_ = importer.Wait()
		}
		return "", startErr
	}
	// Once tar exits, the pipe is closed and docker import ends on its own.
	archiveDone := archive.Wait()
	importDone := importer.Wait()
	image := strings.TrimSpace(id.String())
	switch {
	case archiveDone != nil:
		if importDone == nil && image != "" {
			_ = run(context.WithoutCancel(ctx), "docker", "image", "rm", image)
		}
		return "", fmt.Errorf("tar: %w: %s", archiveDone, strings.TrimSpace(archiveErr.String()))
	case importDone != nil:
		return "", fmt.Errorf("docker import: %w: %s", importDone, strings.TrimSpace(importErr.String()))
	case !strings.HasPrefix(image, "sha256:"):
		return "", fmt.Errorf("docker import printed %q, not an image ID", image)
	}
	return image, nil
}
