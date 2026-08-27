package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
)

const maximumRetirementQueueSize = 1 << 20

var managedRunnerName = regexp.MustCompile(`^runner-[a-f0-9]{8}$`)

type retirementQueue struct {
	mu    sync.Mutex
	file  string
	names map[string]struct{}
}

type retirementLedger struct {
	Version int      `json:"version"`
	Runners []string `json:"runners"`
}

func newRetirementQueue(file string) (*retirementQueue, error) {
	if file == "" {
		return nil, fmt.Errorf("runner retirement queue file is required")
	}
	queue := &retirementQueue{file: file, names: make(map[string]struct{})}
	info, err := os.Lstat(file) // #nosec G304 -- operator-selected durable controller path
	if errors.Is(err, os.ErrNotExist) {
		if err := queue.persist(); err != nil {
			return nil, err
		}
		return queue, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect runner retirement queue: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("runner retirement queue must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("runner retirement queue must not be accessible by group or others")
	}
	if info.Size() > maximumRetirementQueueSize {
		return nil, fmt.Errorf("runner retirement queue exceeds %d bytes", maximumRetirementQueueSize)
	}
	data, err := os.ReadFile(file) // #nosec G304 -- operator-selected durable controller path
	if err != nil {
		return nil, fmt.Errorf("read runner retirement queue: %w", err)
	}
	if len(data) > maximumRetirementQueueSize {
		return nil, fmt.Errorf("runner retirement queue exceeds %d bytes", maximumRetirementQueueSize)
	}
	var ledger retirementLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return nil, fmt.Errorf("parse runner retirement queue: %w", err)
	}
	if ledger.Version != 1 {
		return nil, fmt.Errorf("unsupported runner retirement queue version %d", ledger.Version)
	}
	for _, name := range ledger.Runners {
		if !managedRunnerName.MatchString(name) {
			return nil, fmt.Errorf("invalid managed runner name %q in retirement queue", name)
		}
		if _, duplicate := queue.names[name]; duplicate {
			return nil, fmt.Errorf("duplicate runner %q in retirement queue", name)
		}
		queue.names[name] = struct{}{}
	}
	return queue, nil
}

func (q *retirementQueue) add(name string) error {
	if !managedRunnerName.MatchString(name) {
		return fmt.Errorf("refusing to retire unmanaged runner %q", name)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.names[name]; exists {
		return nil
	}
	q.names[name] = struct{}{}
	if err := q.persistLocked(); err != nil {
		delete(q.names, name)
		return err
	}
	return nil
}

func (q *retirementQueue) remove(name string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, exists := q.names[name]; !exists {
		return nil
	}
	delete(q.names, name)
	if err := q.persistLocked(); err != nil {
		q.names[name] = struct{}{}
		return err
	}
	return nil
}

func (q *retirementQueue) all() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	names := make([]string, 0, len(q.names))
	for name := range q.names {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (q *retirementQueue) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.names)
}

func (q *retirementQueue) persist() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.persistLocked()
}

func (q *retirementQueue) persistLocked() error {
	directory := filepath.Dir(q.file)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create runner retirement queue directory: %w", err)
	}
	names := make([]string, 0, len(q.names))
	for name := range q.names {
		names = append(names, name)
	}
	slices.Sort(names)
	data, err := json.MarshalIndent(retirementLedger{Version: 1, Runners: names}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runner retirement queue: %w", err)
	}
	temporary := q.file + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- operator-selected durable controller path
	if err != nil {
		return fmt.Errorf("open runner retirement queue: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure runner retirement queue: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("write runner retirement queue: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync runner retirement queue: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close runner retirement queue: %w", err)
	}
	if err := os.Rename(temporary, q.file); err != nil {
		return fmt.Errorf("commit runner retirement queue: %w", err)
	}
	directoryHandle, err := os.Open(directory) // #nosec G304 -- operator-selected queue directory
	if err != nil {
		return fmt.Errorf("open runner retirement queue directory: %w", err)
	}
	defer directoryHandle.Close()
	if err := directoryHandle.Sync(); err != nil {
		return fmt.Errorf("sync runner retirement queue directory: %w", err)
	}
	return nil
}
