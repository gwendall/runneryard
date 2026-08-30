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

// departure remembers a worker that left provider inventory before GitHub
// reported its job finished, so the completion message that follows can be
// matched to it instead of being reported as an unknown runner.
type departure struct {
	Worker provider.Worker
	Busy   bool
	At     time.Time
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
	s.departed[record.Worker.RunnerName] = departure{Worker: record.Worker, Busy: record.Busy, At: at}
	s.mu.Unlock()
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
