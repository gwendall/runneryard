package controller

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gwendall/runneryard/provider"
)

// conditionLinesPerAlert bounds one alert; further changes are only counted.
const conditionLinesPerAlert = 10

// conditionWatch reports the compute's conditions, such as a pool host that
// stopped answering. A condition is logged when it appears and when it
// clears. With an alert webhook, each sample's changes go out in one message,
// with a reminder at most hourly while a condition lasts.
type conditionWatch struct {
	logger  *slog.Logger
	alerter *alerter

	mu     sync.Mutex
	active map[string]*watchedCondition
}

type watchedCondition struct {
	condition provider.Condition
	notified  time.Time
}

func (w *conditionWatch) observe(conditions []provider.Condition, now time.Time) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active == nil {
		w.active = make(map[string]*watchedCondition)
	}
	lines := make([]string, 0)
	present := make(map[string]bool, len(conditions))
	for _, condition := range conditions {
		if condition.Key == "" || present[condition.Key] {
			continue
		}
		present[condition.Key] = true
		watched, ok := w.active[condition.Key]
		if !ok {
			w.active[condition.Key] = &watchedCondition{condition: condition, notified: now}
			w.logger.Warn("compute condition", "condition", condition.Key, "summary", condition.Summary, "action", condition.Action, "since", condition.Since)
			lines = append(lines, conditionLine(condition, ""))
			continue
		}
		watched.condition = condition
		if now.Sub(watched.notified) >= alertRepeatInterval {
			watched.notified = now
			lines = append(lines, conditionLine(condition, fmt.Sprintf(" (still, for %s)", roughly(now.Sub(condition.Since)))))
		}
	}
	gone := make([]string, 0)
	for key := range w.active {
		if !present[key] {
			gone = append(gone, key)
		}
	}
	sort.Strings(gone)
	for _, key := range gone {
		condition := w.active[key].condition
		delete(w.active, key)
		lasted := roughly(now.Sub(condition.Since))
		w.logger.Info("compute condition cleared", "condition", key, "lasted", lasted)
		lines = append(lines, fmt.Sprintf("cleared after %s: %s.", lasted, condition.Summary))
	}
	if len(lines) > 0 && w.alerter != nil {
		w.alerter.enqueue(w.alerter.conditionMessage(lines))
	}
}

func conditionLine(condition provider.Condition, note string) string {
	line := condition.Summary + note + "."
	if condition.Action != "" {
		line += " " + condition.Action
	}
	return line
}

// conditionMessage puts one sample's condition changes in one message, with
// the same prefix as the health alerts.
func (a *alerter) conditionMessage(lines []string) string {
	var text strings.Builder
	fmt.Fprintf(&text, "RunnerYard %s (%s): ", a.controller, a.scaleSet)
	if len(lines) == 1 {
		text.WriteString(lines[0])
		return text.String()
	}
	fmt.Fprintf(&text, "%d compute conditions changed:", len(lines))
	for i, line := range lines {
		if i == conditionLinesPerAlert {
			fmt.Fprintf(&text, "\n- and %d more; see runneryard status.", len(lines)-i)
			break
		}
		text.WriteString("\n- " + line)
	}
	return text.String()
}

// roughly formats a duration for a message: "45s", "10m", "2h05m".
func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return max(d, 0).Round(time.Second).String()
	case d < time.Hour:
		return fmt.Sprintf("%dm", d.Round(time.Minute)/time.Minute)
	default:
		d = d.Round(time.Minute)
		return fmt.Sprintf("%dh%02dm", d/time.Hour, d%time.Hour/time.Minute)
	}
}
