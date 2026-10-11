package hostagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const maxStderrBytes = 4 << 10

// CLI runs the docker binary.
type CLI struct {
	// Path is the docker binary; empty means "docker" on PATH.
	Path string
}

func (c CLI) Run(ctx context.Context, env []string, args ...string) ([]byte, error) {
	binary := c.Path
	if binary == "" {
		binary = "docker"
	}
	command := exec.CommandContext(ctx, binary, args...) // #nosec G204 -- fixed docker binary, discrete validated arguments, no shell
	command.Env = append(os.Environ(), env...)
	var stdout bytes.Buffer
	stderr := &boundedBuffer{limit: maxStderrBytes}
	command.Stdout = &stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return stdout.Bytes(), err
		}
		return stdout.Bytes(), errors.New(message)
	}
	return stdout.Bytes(), nil
}

// FreeBytes reports the space available to unprivileged writers on the
// filesystem holding path.
func FreeBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Bsize <= 0 {
		return 0, errors.New("filesystem reports no block size")
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}

// SlotLock returns a lock that serializes start and remove per slot with an
// advisory file lock in dir.
func SlotLock(dir string) func(int) (func(), error) {
	return func(slot int) (func(), error) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		path := filepath.Join(dir, "slot-"+strconv.Itoa(slot)+".lock")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- fixed lock directory and numeric slot
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil { // #nosec G115 -- a file descriptor fits in int
			_ = file.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		return func() {
			_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) // #nosec G115 -- a file descriptor fits in int
			_ = file.Close()
		}, nil
	}
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	if room := b.limit - b.Len(); room > 0 {
		b.Buffer.Write(data[:min(len(data), room)])
	}
	return len(data), nil
}
