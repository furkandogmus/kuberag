package controller

import (
	"time"

	cron "github.com/robfig/cron/v3"
)

// cronParser accepts standard 5-field cron expressions.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// nextFire returns the next scheduled time after `from` for the given cron
// expression, or zero time if the expression is empty or invalid.
func nextFire(expr string, from time.Time) time.Time {
	if expr == "" {
		return time.Time{}
	}
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return time.Time{}
	}
	return sched.Next(from)
}

// cronDue reports whether a cron schedule was due at or before `now`, given the
// last time the action ran. If last is zero the action has never run and is due
// as soon as the schedule produces a fire time at/after creation.
func cronDue(expr string, last, now time.Time) bool {
	if expr == "" {
		return false
	}
	sched, err := cronParser.Parse(expr)
	if err != nil {
		return false
	}
	if last.IsZero() {
		// Never ran: due once the first fire time from a minute ago has passed.
		return !sched.Next(now.Add(-time.Minute)).After(now)
	}
	return !sched.Next(last).After(now)
}

// requeueFor computes a sensible RequeueAfter so the controller wakes near the
// next scheduled freshness/eval fire, clamped to a sane window.
func requeueFor(times ...time.Time) time.Duration {
	const (
		minWait = 30 * time.Second
		maxWait = 10 * time.Minute
	)
	now := time.Now()
	best := now.Add(maxWait)
	for _, t := range times {
		if t.IsZero() {
			continue
		}
		if t.Before(best) && t.After(now) {
			best = t
		}
	}
	d := time.Until(best)
	if d < minWait {
		return minWait
	}
	if d > maxWait {
		return maxWait
	}
	return d
}
