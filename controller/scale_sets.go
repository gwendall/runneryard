package controller

import (
	"context"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/gwendall/runneryard/provider"
)

// scaleSetRoute is one runner scale set the scaler serves: the main one,
// placed by the compute's placement order, or the optional ephemeral one,
// whose workers start on the ephemeral tier alone so that a job can skip the
// pools without taking them out of placement. GitHub gives a scale set's job
// to whichever of its runners comes online first, so only a second scale set
// can route a job to a different kind of worker.
type scaleSetRoute struct {
	name       string
	id         int
	minWorkers int
	// launch starts this scale set's workers. Inventory and Destroy always
	// go through the scaler's compute, which covers every tier.
	launch provider.Compute
}

func (s *scaler) mainRoute() scaleSetRoute {
	return scaleSetRoute{name: s.scaleSetName, id: s.scaleSetID, minWorkers: s.minWorkers, launch: s.compute}
}

// servesScaleSet reports whether id is a scale set this controller
// registered. A runner registration anywhere else is never removed.
func (s *scaler) servesScaleSet(id int) bool {
	return id == s.scaleSetID || (s.ephemeral != nil && id == s.ephemeral.id)
}

// routeCount counts the workers of one scale set.
func (s *scaler) routeCount(route scaleSetRoute) int {
	current, _ := s.routeCounts(route)
	return current
}

// routeCounts returns the workers of one scale set and of the whole fleet
// from one snapshot. A worker that names no scale set this controller serves
// counts toward the main one, as every worker did before there were two, so
// MAX_RUNNERS still covers it. With a single scale set both are the fleet.
func (s *scaler) routeCounts(route scaleSetRoute) (current, fleet int) {
	if s.ephemeral == nil {
		fleet = s.state.count()
		return fleet, fleet
	}
	ephemeral, fleet := s.state.countScaleSet(s.ephemeral.id)
	if route.id == s.ephemeral.id {
		return ephemeral, fleet
	}
	return fleet - ephemeral, fleet
}

// routeWorker returns whether a worker belongs to the route's scale set,
// counting a worker that names no served scale set toward the main one, as
// routeCounts does.
func (s *scaler) routeWorker(route scaleSetRoute) func(provider.Worker) bool {
	if s.ephemeral == nil {
		return func(provider.Worker) bool { return true }
	}
	ephemeralID := s.ephemeral.id
	if route.id == ephemeralID {
		return func(worker provider.Worker) bool { return worker.RunnerScaleSetID == ephemeralID }
	}
	return func(worker provider.Worker) bool { return worker.RunnerScaleSetID != ephemeralID }
}

// reportScaleSets publishes each scale set's worker count when there are two.
func (s *scaler) reportScaleSets() {
	if s.ephemeral == nil {
		return
	}
	ephemeral, total := s.state.countScaleSet(s.ephemeral.id)
	s.reporter.scaleSetWorkers(s.scaleSetName, total-ephemeral)
	s.reporter.scaleSetWorkers(s.ephemeral.name, ephemeral)
}

// scaleSetSessions describes the sessions of both scale sets, main first.
// Each takes its own copy of the launch gate's wake-ups; only the main one
// takes the pools' room signal, since only the main scale set speculates.
func (s *scaler) scaleSetSessions(open func(scaleSetID int) sessionOpener, wakes []chan struct{}) []sessionScaleSet {
	return []sessionScaleSet{
		{name: s.scaleSetName, id: s.scaleSetID, open: open(s.scaleSetID), handler: scaleSetHandler{scaler: s, route: s.mainRoute()}, wake: wakes[0], room: s.roomReady()},
		{name: s.ephemeral.name, id: s.ephemeral.id, open: open(s.ephemeral.id), handler: scaleSetHandler{scaler: s, route: *s.ephemeral}, wake: wakes[1]},
	}
}

// scaleSetHandler is the listener.Scaler of one scale set when the controller
// serves two. The listeners take turns on handlerMu, so the scaler handles one
// stream of events as with a single scale set, and every launch is checked
// against a fleet that cannot change underneath it.
type scaleSetHandler struct {
	scaler *scaler
	route  scaleSetRoute
}

func (h scaleSetHandler) HandleDesiredRunnerCount(ctx context.Context, assignedJobs int) (int, error) {
	h.scaler.handlerMu.Lock()
	defer h.scaler.handlerMu.Unlock()
	count, err := h.scaler.handleDesiredRunnerCount(ctx, h.route, assignedJobs)
	return count, failClosed(err)
}

func (h scaleSetHandler) HandleJobStarted(ctx context.Context, job *scaleset.JobStarted) error {
	h.scaler.handlerMu.Lock()
	defer h.scaler.handlerMu.Unlock()
	return failClosed(h.scaler.handleJobStarted(ctx, job))
}

func (h scaleSetHandler) HandleJobCompleted(ctx context.Context, job *scaleset.JobCompleted) error {
	h.scaler.handlerMu.Lock()
	defer h.scaler.handlerMu.Unlock()
	return failClosed(h.scaler.handleJobCompleted(ctx, job))
}

// fanOutWake hands each launch-gate wake-up to every scale set's session. The
// gate is shared, and the probe belongs to whichever scale set has jobs
// waiting; a session without any answers the wake-up with an empty poll.
func fanOutWake(ctx context.Context, source <-chan struct{}, targets []chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-source:
		}
		for _, target := range targets {
			select {
			case target <- struct{}{}:
			default:
			}
		}
	}
}

var _ listener.Scaler = scaleSetHandler{}
