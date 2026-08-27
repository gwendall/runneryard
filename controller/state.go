package controller

import (
	"sync"

	"github.com/gwendall/runneryard/provider"
)

type workerRecord struct {
	Worker provider.Worker
	Busy   bool
}

type workerState struct {
	mu      sync.Mutex
	workers map[string]workerRecord
}

func newWorkerState() *workerState {
	return &workerState{workers: make(map[string]workerRecord)}
}

func (s *workerState) add(worker provider.Worker, busy bool) {
	s.mu.Lock()
	s.workers[worker.RunnerName] = workerRecord{Worker: worker, Busy: busy}
	s.mu.Unlock()
}

func (s *workerState) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.workers)
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
	s.workers[name] = record
	return true
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
