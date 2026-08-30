package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

type usageBudget struct {
	mu          sync.Mutex
	limit       time.Duration
	window      time.Duration
	reservation time.Duration
	stateFile   string
	entries     []usageEntry
}

type usageEntry struct {
	LeaseID   string    `json:"lease_id"`
	ChargedAt time.Time `json:"charged_at"`
	StartedAt time.Time `json:"started_at"`
	Seconds   int64     `json:"seconds"`
	Active    bool      `json:"active"`
	Confirmed bool      `json:"confirmed"`
}

type usageLedger struct {
	Version int          `json:"version"`
	Entries []usageEntry `json:"entries"`
}

func newUsageBudget(limit, window time.Duration, stateFile string, reservation time.Duration) (*usageBudget, error) {
	if limit <= 0 || window <= 0 || stateFile == "" || reservation <= 0 || reservation > limit {
		return nil, fmt.Errorf("runner usage budget, window, and state file are required")
	}
	b := &usageBudget{limit: limit, window: window, reservation: reservation, stateFile: stateFile}
	// stateFile is an operator-selected durable controller path. The controller
	// OS identity is the filesystem security boundary.
	data, err := os.ReadFile(stateFile) // #nosec G304
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("runner usage budget is not initialized; run runneryard budget init --file %s", stateFile)
	}
	if err != nil {
		return nil, fmt.Errorf("read runner usage budget: %w", err)
	}
	var ledger usageLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return nil, fmt.Errorf("parse runner usage budget: %w", err)
	}
	if ledger.Version != 1 {
		return nil, fmt.Errorf("unsupported runner usage budget version %d", ledger.Version)
	}
	b.entries = ledger.Entries
	if err := b.validate(); err != nil {
		return nil, err
	}
	if b.prune(time.Now()) {
		if err := b.persist(); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// InitializeUsageBudget creates a new empty ledger and refuses to reset an
// existing deployment. It is intentionally separate from controller startup.
func InitializeUsageBudget(stateFile string) error {
	if stateFile == "" {
		return fmt.Errorf("runner usage budget state file is required")
	}
	directory := filepath.Dir(stateFile)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create runner usage budget directory: %w", err)
	}
	data, err := json.MarshalIndent(usageLedger{Version: 1, Entries: nil}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runner usage budget: %w", err)
	}
	file, err := os.OpenFile(stateFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("refusing to reset existing runner usage budget at %s", stateFile)
	}
	if err != nil {
		return fmt.Errorf("create runner usage budget: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("write and close initial runner usage budget: %v; %w", err, closeErr)
		}
		return fmt.Errorf("write initial runner usage budget: %w", err)
	}
	if err := file.Sync(); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("sync and close initial runner usage budget: %v; %w", err, closeErr)
		}
		return fmt.Errorf("sync initial runner usage budget: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close initial runner usage budget: %w", err)
	}
	directoryHandle, err := os.Open(directory) // #nosec G304 -- operator-selected ledger directory
	if err != nil {
		return fmt.Errorf("open runner usage budget directory: %w", err)
	}
	defer directoryHandle.Close()
	if err := directoryHandle.Sync(); err != nil {
		return fmt.Errorf("sync runner usage budget directory: %w", err)
	}
	return nil
}

func (b *usageBudget) reserve(leaseID string, now time.Time, maximum time.Duration) (bool, time.Time, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.prune(now)
	for _, entry := range b.entries {
		if entry.LeaseID == leaseID {
			return false, time.Time{}, fmt.Errorf("runner usage budget already contains lease %s", leaseID)
		}
	}
	requested := durationSeconds(maximum)
	if requested != durationSeconds(b.reservation) {
		return false, time.Time{}, fmt.Errorf("runner reservation must equal configured maximum lifetime %s", b.reservation)
	}
	used := b.usedSeconds()
	limit := durationSeconds(b.limit)
	if used > limit || requested > limit-used {
		return false, b.nextAvailable(now), nil
	}
	b.entries = append(b.entries, usageEntry{
		LeaseID: leaseID, ChargedAt: now.UTC(), StartedAt: now.UTC(), Seconds: requested, Active: true,
	})
	if err := b.persist(); err != nil {
		b.entries = b.entries[:len(b.entries)-1]
		return false, time.Time{}, err
	}
	return true, time.Time{}, nil
}

// settle charges a lease for the time between its start and now.
func (b *usageBudget) settle(leaseID string, now time.Time) error {
	return b.settleAt(leaseID, now, now)
}

// settleAt charges a lease for the worker's actual lifetime, from its start to
// stoppedAt, capped at the reservation. A zero or earlier-than-start stoppedAt
// falls back to now. The charge is recorded at now so window expiry follows
// the settlement, not the stop.
func (b *usageBudget) settleAt(leaseID string, stoppedAt, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	index := slices.IndexFunc(b.entries, func(entry usageEntry) bool { return entry.LeaseID == leaseID && entry.Active })
	if index < 0 {
		return nil
	}
	previousEntries := slices.Clone(b.entries)
	previous := b.entries[index]
	if stoppedAt.IsZero() || stoppedAt.Before(previous.StartedAt) || stoppedAt.After(now) {
		stoppedAt = now
	}
	b.entries[index].Active = false
	b.entries[index].ChargedAt = now.UTC()
	b.entries[index].Seconds = min(previous.Seconds, max(1, durationSeconds(stoppedAt.Sub(previous.StartedAt))))
	b.prune(now)
	if err := b.persist(); err != nil {
		b.entries = previousEntries
		return err
	}
	return nil
}

func (b *usageBudget) forfeit(leaseID string, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	index := slices.IndexFunc(b.entries, func(entry usageEntry) bool { return entry.LeaseID == leaseID && entry.Active })
	if index < 0 {
		return nil
	}
	previous := slices.Clone(b.entries)
	b.entries[index].Active = false
	b.entries[index].ChargedAt = now.UTC()
	if err := b.persist(); err != nil {
		b.entries = previous
		return err
	}
	return nil
}

func (b *usageBudget) release(leaseID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	index := slices.IndexFunc(b.entries, func(entry usageEntry) bool { return entry.LeaseID == leaseID && entry.Active })
	if index < 0 {
		return nil
	}
	previous := slices.Clone(b.entries)
	b.entries = slices.Delete(b.entries, index, index+1)
	if err := b.persist(); err != nil {
		b.entries = previous
		return err
	}
	return nil
}

func (b *usageBudget) adopt(leaseID string, startedAt time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if leaseID == "" {
		return fmt.Errorf("cannot adopt a worker without a lease ID")
	}
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	for index := range b.entries {
		entry := &b.entries[index]
		if entry.LeaseID != leaseID {
			continue
		}
		if entry.Active {
			if entry.Confirmed {
				return nil
			}
			previous := *entry
			entry.Confirmed = true
			entry.Seconds = durationSeconds(b.reservation)
			if err := b.persist(); err != nil {
				*entry = previous
				return err
			}
			return nil
		}
		previous := *entry
		entry.Active = true
		entry.StartedAt = startedAt.UTC()
		entry.ChargedAt = startedAt.UTC()
		entry.Seconds = durationSeconds(b.reservation)
		entry.Confirmed = true
		if err := b.persist(); err != nil {
			*entry = previous
			return err
		}
		return nil
	}
	b.entries = append(b.entries, usageEntry{
		LeaseID: leaseID, ChargedAt: startedAt.UTC(), StartedAt: startedAt.UTC(), Seconds: durationSeconds(b.reservation), Active: true, Confirmed: true,
	})
	return b.persist()
}

func (b *usageBudget) reconcile(activeLeases map[string]struct{}, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	previous := slices.Clone(b.entries)
	changed := false
	for index := range b.entries {
		entry := &b.entries[index]
		if !entry.Active {
			continue
		}
		if _, ok := activeLeases[entry.LeaseID]; ok {
			continue
		}
		entry.Active = false
		entry.ChargedAt = now.UTC()
		if entry.Confirmed {
			entry.Seconds = min(entry.Seconds, max(1, durationSeconds(now.Sub(entry.StartedAt))))
		}
		changed = true
	}
	if b.prune(now) {
		changed = true
	}
	if !changed {
		return nil
	}
	if err := b.persist(); err != nil {
		b.entries = previous
		return err
	}
	return nil
}

func (b *usageBudget) prune(now time.Time) bool {
	changed := false
	for index := range b.entries {
		entry := &b.entries[index]
		if entry.Active && !now.Before(entry.StartedAt.Add(time.Duration(entry.Seconds)*time.Second)) {
			entry.Active = false
			entry.ChargedAt = entry.StartedAt.Add(time.Duration(entry.Seconds) * time.Second)
			changed = true
		}
	}
	cutoff := now.Add(-b.window)
	before := len(b.entries)
	b.entries = slices.DeleteFunc(b.entries, func(entry usageEntry) bool {
		return !entry.Active && entry.ChargedAt.Before(cutoff)
	})
	return changed || len(b.entries) != before
}

func (b *usageBudget) usedSeconds() int64 {
	var total int64
	for _, entry := range b.entries {
		total += entry.Seconds
	}
	return total
}

func (b *usageBudget) snapshot(now time.Time) BudgetStatus {
	b.mu.Lock()
	defer b.mu.Unlock()

	cutoff := now.Add(-b.window)
	var used, reserved int64
	for _, entry := range b.entries {
		if !entry.Active && entry.ChargedAt.Before(cutoff) {
			continue
		}
		if entry.Active {
			reserved += entry.Seconds
		} else {
			used += entry.Seconds
		}
	}
	limit := durationSeconds(b.limit)
	remaining := max(int64(0), limit-used-reserved)
	status := BudgetStatus{
		LimitSeconds: limit, UsedSeconds: used, ReservedSeconds: reserved,
		RemainingSeconds: remaining, WindowSeconds: durationSeconds(b.window),
	}
	status.BurnSecondsPerDay = b.burnPerDay(now)
	if status.BurnSecondsPerDay > 0 {
		status.HorizonSeconds = remaining * int64(24*time.Hour/time.Second) / status.BurnSecondsPerDay
	}
	if remaining < durationSeconds(b.reservation) {
		status.RefusalReason = "usage_budget_exhausted"
		status.NextAvailableAt = b.nextAvailable(now).UTC()
	}
	return status
}

// burnPerDay extrapolates the settled usage of the trailing day to a daily
// rate. Ledgers younger than an hour are extrapolated from one hour so a few
// minutes of history do not produce a wild horizon.
func (b *usageBudget) burnPerDay(now time.Time) int64 {
	cutoff := now.Add(-24 * time.Hour)
	var recent int64
	oldest := now
	for _, entry := range b.entries {
		if entry.Active || entry.ChargedAt.Before(cutoff) {
			continue
		}
		recent += entry.Seconds
		if entry.ChargedAt.Before(oldest) {
			oldest = entry.ChargedAt
		}
	}
	if recent == 0 {
		return 0
	}
	span := max(now.Sub(oldest), time.Hour)
	return int64(float64(recent) * float64(24*time.Hour) / float64(span))
}

func (b *usageBudget) validate() error {
	seen := make(map[string]struct{}, len(b.entries))
	var total int64
	for index, entry := range b.entries {
		if entry.LeaseID == "" || entry.StartedAt.IsZero() || entry.ChargedAt.IsZero() {
			return fmt.Errorf("invalid runner usage budget entry %d: identity and timestamps are required", index)
		}
		if _, ok := seen[entry.LeaseID]; ok {
			return fmt.Errorf("invalid runner usage budget: duplicate lease %s", entry.LeaseID)
		}
		seen[entry.LeaseID] = struct{}{}
		if entry.Seconds < 1 || entry.Seconds > durationSeconds(b.reservation) {
			return fmt.Errorf("invalid runner usage budget entry %s: seconds must be between 1 and %d", entry.LeaseID, durationSeconds(b.reservation))
		}
		if entry.Active && entry.Seconds != durationSeconds(b.reservation) {
			return fmt.Errorf("invalid runner usage budget entry %s: active reservation is undercharged", entry.LeaseID)
		}
		if !entry.Active && entry.ChargedAt.Before(entry.StartedAt) {
			return fmt.Errorf("invalid runner usage budget entry %s: charge precedes start", entry.LeaseID)
		}
		if total > math.MaxInt64-entry.Seconds {
			return fmt.Errorf("invalid runner usage budget: usage overflow")
		}
		total += entry.Seconds
	}
	return nil
}

func (b *usageBudget) nextAvailable(now time.Time) time.Time {
	completed := make([]usageEntry, 0, len(b.entries))
	for _, entry := range b.entries {
		if !entry.Active {
			completed = append(completed, entry)
		}
	}
	if len(completed) == 0 {
		return now.Add(b.window)
	}
	slices.SortFunc(completed, func(a, c usageEntry) int { return a.ChargedAt.Compare(c.ChargedAt) })
	return completed[0].ChargedAt.Add(b.window)
}

func (b *usageBudget) persist() error {
	directory := filepath.Dir(b.stateFile)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create runner usage budget directory: %w", err)
	}
	data, err := json.MarshalIndent(usageLedger{Version: 1, Entries: b.entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runner usage budget: %w", err)
	}
	temporary := b.stateFile + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304
	if err != nil {
		return fmt.Errorf("open runner usage budget: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("write and close runner usage budget: %v; %w", err, closeErr)
		}
		return fmt.Errorf("write runner usage budget: %w", err)
	}
	if err := file.Sync(); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("sync and close runner usage budget: %v; %w", err, closeErr)
		}
		return fmt.Errorf("sync runner usage budget: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close runner usage budget: %w", err)
	}
	if err := os.Rename(temporary, b.stateFile); err != nil {
		return fmt.Errorf("commit runner usage budget: %w", err)
	}
	directoryHandle, err := os.Open(directory) // #nosec G304 -- operator-selected ledger directory
	if err != nil {
		return fmt.Errorf("open runner usage budget directory: %w", err)
	}
	defer directoryHandle.Close()
	if err := directoryHandle.Sync(); err != nil {
		return fmt.Errorf("sync runner usage budget directory: %w", err)
	}
	return nil
}

func durationSeconds(value time.Duration) int64 {
	return int64(math.Ceil(value.Seconds()))
}
