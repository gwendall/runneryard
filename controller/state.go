package controller

import (
	"sync"
	"time"

	"github.com/gwendall/runneryard/provider"
)

type workerRecord struct {
	Worker       provider.Worker
	Busy         bool
	Observed     bool
	MissingSince time.Time
}

// departure remembers a worker that left provider inventory, or was retired
// as stopped, before GitHub reported its job finished, so the messages that
// follow can be matched to it instead of being reported as an unknown runner.
type departure struct {
	Worker provider.Worker
	Busy   bool
	At     time.Time
	// Stopped marks a worker retired because the provider reported it
	// stopped. Awaiting marks one whose completion is expected: if it never
	// comes, the stop was a fault and is reported once.
	Stopped  bool
	Awaiting bool
}

type workerState struct {
	mu       sync.Mutex
	workers  map[string]workerRecord
	departed map[string]departure
}

func newWorkerState() *workerState {
	return &workerState{workers: make(map[string]workerRecord), departed: make(map[string]departure)}
}

func (s *workerState) markDeparted(record workerRecord, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if known, ok := s.departed[record.Worker.RunnerName]; ok && known.Stopped {
		// Retired as stopped already; the wait for its completion goes on.
		return
	}
	s.departed[record.Worker.RunnerName] = departure{Worker: record.Worker, Busy: record.Busy, At: at}
}

// markStopped remembers a worker retired as stopped. A worker already
// remembered keeps its first stop, so a retirement that is retried does not
// push back the wait for its completion.
func (s *workerState) markStopped(record workerRecord, at time.Time, awaiting bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if known, ok := s.departed[record.Worker.RunnerName]; ok && known.Stopped {
		return
	}
	s.departed[record.Worker.RunnerName] = departure{Worker: record.Worker, Busy: record.Busy, At: at, Stopped: true, Awaiting: awaiting}
}

// markDepartedBusy records that the job of a remembered worker started; its
// completion is still to come, so the departure is kept.
func (s *workerState) markDepartedBusy(name string) (departure, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.departed[name]
	if ok {
		record.Busy = true
		s.departed[name] = record
	}
	return record, ok
}

// takeOverdue returns, once each, the stopped workers whose completion has
// been awaited since before the given time. They stay remembered, so a late
// completion is still matched.
func (s *workerState) takeOverdue(before time.Time) []departure {
	s.mu.Lock()
	defer s.mu.Unlock()
	var overdue []departure
	for name, record := range s.departed {
		if record.Awaiting && record.At.Before(before) {
			record.Awaiting = false
			s.departed[name] = record
			overdue = append(overdue, record)
		}
	}
	return overdue
}

func (s *workerState) takeDeparted(name string) (departure, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.departed[name]
	delete(s.departed, name)
	return record, ok
}

func (s *workerState) pruneDeparted(before time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, record := range s.departed {
		if record.At.Before(before) {
			delete(s.departed, name)
		}
	}
}

func (s *workerState) add(worker provider.Worker, busy bool) {
	s.mu.Lock()
	s.workers[worker.RunnerName] = workerRecord{Worker: worker, Busy: busy, Observed: true}
	s.mu.Unlock()
}

func (s *workerState) adopt(worker provider.Worker) {
	s.mu.Lock()
	s.workers[worker.RunnerName] = workerRecord{Worker: worker}
	s.mu.Unlock()
}

func (s *workerState) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.workers)
}

// countScaleSet returns how many workers carry the scale set id, and the
// whole fleet, from one snapshot.
func (s *workerState) countScaleSet(id int) (matching, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range s.workers {
		if record.Worker.RunnerScaleSetID == id {
			matching++
		}
	}
	return matching, len(s.workers)
}

func (s *workerState) summary() (actual, busy, idle, unknown int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	actual = len(s.workers)
	for _, record := range s.workers {
		if !record.Observed {
			unknown++
		} else if record.Busy {
			busy++
		} else {
			idle++
		}
	}
	return actual, busy, idle, unknown
}

// idleByStart counts, among the workers include accepts, those that run a job
// or may run one (adopted and never observed), and the observed idle workers
// split by whether fast says their runner comes online in seconds.
func (s *workerState) idleByStart(include func(provider.Worker) bool, fast func(workerID string) bool) (busy, fastIdle, slowIdle int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range s.workers {
		switch {
		case !include(record.Worker):
			continue
		case record.Busy || !record.Observed:
			busy++
		case fast(record.Worker.ID):
			fastIdle++
		default:
			slowIdle++
		}
	}
	return busy, fastIdle, slowIdle
}

func (s *workerState) all() map[string]workerRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]workerRecord, len(s.workers))
	for name, record := range s.workers {
		result[name] = record
	}
	return result
}

func (s *workerState) markBusy(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.workers[name]
	if !ok {
		return false
	}
	record.Busy = true
	record.Observed = true
	record.MissingSince = time.Time{}
	s.workers[name] = record
	return true
}

func (s *workerState) markMissing(name string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.workers[name]
	if !ok {
		return false
	}
	if record.MissingSince.IsZero() {
		record.MissingSince = at
		s.workers[name] = record
	}
	return true
}

func (s *workerState) markPresent(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.workers[name]
	if !ok || record.MissingSince.IsZero() {
		return
	}
	record.MissingSince = time.Time{}
	s.workers[name] = record
}

func (s *workerState) get(name string) (workerRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.workers[name]
	return record, ok
}

func (s *workerState) remove(name string) (workerRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.workers[name]
	delete(s.workers, name)
	return record, ok
}
