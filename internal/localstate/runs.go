package localstate

import "errors"

// Runs returns a copy of every locally-tracked Codex managed run, keyed by
// run ID.
func (s *Store) Runs() (map[string]Run, error) {
	d, err := s.view()
	if err != nil {
		return nil, err
	}
	result := make(map[string]Run, len(d.Runs))
	for id, r := range d.Runs {
		result[id] = toRun(r)
	}
	return result, nil
}

// StartRun records a newly-started managed run. It fails if a run with the
// same ID is already tracked (run IDs are generated fresh per dispatch, so
// a collision indicates a caller bug, not a legitimate race).
func (s *Store) StartRun(r Run) error {
	return s.update(func(d *data) error {
		if _, ok := d.Runs[r.ID]; ok {
			return errors.New("run already exists")
		}
		d.Runs[r.ID] = fromRun(r)
		return nil
	})
}

// SaveRun upserts a managed run record (e.g. once the supervisor has
// observed its process identity after spawn).
func (s *Store) SaveRun(r Run) error {
	return s.update(func(d *data) error { d.Runs[r.ID] = fromRun(r); return nil })
}

// DeleteRun removes a managed run record outright (used to clean up after
// a run failed to start).
func (s *Store) DeleteRun(id string) error {
	return s.update(func(d *data) error { delete(d.Runs, id); return nil })
}

// MarkRunStopped records that a managed run's process has exited, clearing
// its PID (a dead PID must never be treated as identity later -- see the
// DesignDoc's process-ownership invariants) and returning the updated
// record.
func (s *Store) MarkRunStopped(id string) (Run, error) {
	var stopped Run
	err := s.update(func(d *data) error {
		r := d.Runs[id]
		r.State = "stopped"
		r.PID = 0
		d.Runs[id] = r
		stopped = toRun(r)
		return nil
	})
	return stopped, err
}

// MarkAllRunningStale transitions every run currently recorded as
// running/starting to the terminal "stale" state with reason, used when a
// new supervisor instance starts and cannot recover any PTY the previous
// instance held (see the DesignDoc's Codex supervisor Lifetime section:
// "process を推測して再利用せず、既存 managed run を stale として扱う").
func (s *Store) MarkAllRunningStale(reason string) error {
	return s.update(func(d *data) error {
		for id, r := range d.Runs {
			if r.State == "running" || r.State == "starting" {
				r.State = "stale"
				r.Error = reason
				d.Runs[id] = r
			}
		}
		return nil
	})
}

// DeleteTerminalUnboundRun deletes run id, but only if isTerminal(r.State)
// still holds and the run is still unbound (SessionID == "") at the moment
// of the atomic check -- re-verified here, under the same exclusive lock
// as the delete itself, rather than trusting a caller's earlier read (see
// provider/codex.Provider.Archive, which makes an initial read to decide
// whether to call this at all, then relies on this method's own re-check
// for correctness). If the run no longer matches -- already deleted, bound
// to a thread since, or no longer terminal -- it is left alone and this
// reports no error.
func (s *Store) DeleteTerminalUnboundRun(id string, isTerminal func(state string) bool) error {
	return s.update(func(d *data) error {
		r, ok := d.Runs[id]
		if !ok || r.SessionID != "" || !isTerminal(r.State) {
			return nil
		}
		delete(d.Runs, id)
		return nil
	})
}

// UpdateRunIf conditionally applies mutate to run id: predicate is
// re-evaluated here, under the exclusive lock, against whatever is
// currently persisted -- never against a caller's own earlier read -- and
// the update is applied only if it still returns true. If id is not
// currently tracked, or predicate returns false, nothing is persisted and
// applied reports false; this is not an error, just a caller's decision
// losing to whatever changed (or removed) the record first.
//
// predicate and mutate must be pure functions of the Run they are given --
// no filesystem/process I/O, no capturing external state that could
// change between calls. This is the compare-and-apply half of a
// stale-safe update: a caller with an expensive or I/O-bound decision
// (e.g. provider/codex.Provider.reconcile's Codex writer-lock ownership
// probes) computes that decision against an earlier, unlocked Runs()
// snapshot, entirely outside any localstate lock, then hands the
// already-computed result to mutate and a snapshot-equality check to
// predicate -- so the potentially-slow external observation never runs
// while localstate holds its cross-process exclusive lock, and a run that
// changed in between (bound, deleted, or otherwise mutated by a
// concurrent writer) is never clobbered by a now-stale decision.
func (s *Store) UpdateRunIf(id string, predicate func(current Run) bool, mutate func(current Run) Run) (applied bool, err error) {
	err = s.update(func(d *data) error {
		r, ok := d.Runs[id]
		if !ok || !predicate(toRun(r)) {
			return nil
		}
		d.Runs[id] = fromRun(mutate(toRun(r)))
		applied = true
		return nil
	})
	return applied, err
}
