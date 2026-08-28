package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sync"
)

const maximumRetirementQueueSize = 1 << 20

var managedRunnerName = regexp.MustCompile(`^runner-(?:[a-f0-9]{8}|[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12})$`)

type budgetDisposition string

const (
	settleActualUsage  budgetDisposition = "settle"
	forfeitReservation budgetDisposition = "forfeit"
)

type retirementEntry struct {
	RunnerName        string            `json:"runner_name"`
	RunnerID          int64             `json:"runner_id,omitempty"`
	RunnerScaleSetID  int               `json:"runner_scale_set_id"`
	LeaseID           string            `json:"lease_id"`
	BudgetDisposition budgetDisposition `json:"budget_disposition"`
}

type retirementQueue struct {
	mu      sync.Mutex
	file    string
	entries map[string]retirementEntry
}

type retirementLedger struct {
	Version int               `json:"version"`
	Entries []retirementEntry `json:"entries"`
	Runners []string          `json:"runners,omitempty"`
}

func newRetirementQueue(file string) (*retirementQueue, error) {
	if file == "" {
		return nil, fmt.Errorf("runner retirement queue file is required")
	}
	queue := &retirementQueue{file: file, entries: make(map[string]retirementEntry)}
	ledger, err := loadRetirementLedger(file)
	if errors.Is(err, os.ErrNotExist) {
		if err := queue.persist(); err != nil {
			return nil, err
		}
		return queue, nil
	}
	if err != nil {
		return nil, err
	}
	if ledger.Version == 1 {
		if len(ledger.Runners) > 0 {
			return nil, errors.New("legacy runner retirement queue is non-empty; drain it with the previous controller before upgrading")
		}
		ledger.Version = 2
	}
	if ledger.Version != 2 {
		return nil, fmt.Errorf("unsupported runner retirement queue version %d", ledger.Version)
	}
	for _, entry := range ledger.Entries {
		if err := validateRetirementEntry(entry); err != nil {
			return nil, err
		}
		if _, duplicate := queue.entries[entry.RunnerName]; duplicate {
			return nil, fmt.Errorf("duplicate runner %q in retirement queue", entry.RunnerName)
		}
		queue.entries[entry.RunnerName] = entry
	}
	return queue, nil
}

func loadRetirementLedger(path string) (retirementLedger, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return retirementLedger{}, errors.New("resolve runner retirement queue path")
	}
	root, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return retirementLedger{}, os.ErrNotExist
		}
		return retirementLedger{}, errors.New("open runner retirement queue directory")
	}
	defer root.Close()
	name := filepath.Base(absolute)
	info, err := root.Lstat(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return retirementLedger{}, os.ErrNotExist
		}
		return retirementLedger{}, fmt.Errorf("inspect runner retirement queue: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return retirementLedger{}, errors.New("runner retirement queue must be a regular file, not a symlink")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return retirementLedger{}, errors.New("runner retirement queue is accessible by group or others")
	}
	if info.Size() < 1 || info.Size() > maximumRetirementQueueSize {
		return retirementLedger{}, errors.New("runner retirement queue size is outside the safe range")
	}
	file, err := root.Open(name)
	if err != nil {
		return retirementLedger{}, errors.New("open runner retirement queue")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return retirementLedger{}, errors.New("runner retirement queue changed while it was being inspected")
	}
	var ledger retirementLedger
	decoder := json.NewDecoder(io.LimitReader(file, maximumRetirementQueueSize))
	if err := decoder.Decode(&ledger); err != nil {
		return retirementLedger{}, fmt.Errorf("decode runner retirement queue: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return retirementLedger{}, errors.New("runner retirement queue contains trailing data")
	}
	return ledger, nil
}

func validateRetirementEntry(entry retirementEntry) error {
	if !managedRunnerName.MatchString(entry.RunnerName) {
		return fmt.Errorf("invalid managed runner name %q in retirement queue", entry.RunnerName)
	}
	if entry.RunnerID < 0 || entry.RunnerScaleSetID < 1 || entry.LeaseID == "" {
		return fmt.Errorf("invalid identity proof for runner %q", entry.RunnerName)
	}
	if entry.BudgetDisposition != settleActualUsage && entry.BudgetDisposition != forfeitReservation {
		return fmt.Errorf("invalid budget disposition for runner %q", entry.RunnerName)
	}
	return nil
}

func (q *retirementQueue) put(entry retirementEntry) error {
	if err := validateRetirementEntry(entry); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	previous, exists := q.entries[entry.RunnerName]
	if exists {
		if previous.RunnerScaleSetID != entry.RunnerScaleSetID || previous.LeaseID != entry.LeaseID || previous.BudgetDisposition != entry.BudgetDisposition {
			return fmt.Errorf("refusing to replace identity proof for runner %q", entry.RunnerName)
		}
		if previous.RunnerID != 0 && entry.RunnerID != 0 && previous.RunnerID != entry.RunnerID {
			return fmt.Errorf("refusing to replace registration id for runner %q", entry.RunnerName)
		}
		if entry.RunnerID == 0 {
			entry.RunnerID = previous.RunnerID
		}
	}
	q.entries[entry.RunnerName] = entry
	if err := q.persistLocked(); err != nil {
		if exists {
			q.entries[entry.RunnerName] = previous
		} else {
			delete(q.entries, entry.RunnerName)
		}
		return err
	}
	return nil
}

func (q *retirementQueue) remove(name string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	previous, exists := q.entries[name]
	if !exists {
		return nil
	}
	delete(q.entries, name)
	if err := q.persistLocked(); err != nil {
		q.entries[name] = previous
		return err
	}
	return nil
}

func (q *retirementQueue) all() []retirementEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	entries := make([]retirementEntry, 0, len(q.entries))
	for _, entry := range q.entries {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(left, right retirementEntry) int {
		return compareStrings(left.RunnerName, right.RunnerName)
	})
	return entries
}

func compareStrings(left, right string) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func (q *retirementQueue) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries)
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
	entries := make([]retirementEntry, 0, len(q.entries))
	for _, entry := range q.entries {
		entries = append(entries, entry)
	}
	slices.SortFunc(entries, func(left, right retirementEntry) int {
		return compareStrings(left.RunnerName, right.RunnerName)
	})
	data, err := json.MarshalIndent(retirementLedger{Version: 2, Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runner retirement queue: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".runneryard-retirements-*")
	if err != nil {
		return fmt.Errorf("create temporary runner retirement queue: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure runner retirement queue: %w", err)
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write runner retirement queue: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync runner retirement queue: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close runner retirement queue: %w", err)
	}
	if err := os.Rename(temporaryName, q.file); err != nil {
		return fmt.Errorf("commit runner retirement queue: %w", err)
	}
	if err := os.Chmod(q.file, 0o600); err != nil {
		return fmt.Errorf("secure committed runner retirement queue: %w", err)
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
