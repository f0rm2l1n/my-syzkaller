// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package experiment

import "fmt"

type Verdict string

const (
	VerdictFalsified           Verdict = "falsified"
	VerdictInsufficient        Verdict = "insufficient-evidence"
	VerdictObservedViolation   Verdict = "observed-violation"
	VerdictConfirmedOnOriginal Verdict = "confirmed-on-original"
)

// OracleResult records what the evidence supports. It never upgrades a
// probe-only assertion into a confirmed kernel problem.
type OracleResult struct {
	Verdict       Verdict  `json:"verdict"`
	Reason        string   `json:"reason"`
	ObservedKinds []string `json:"observed_kinds,omitempty"`
}

// EvaluatePredicate dispatches a versioned predicate to the evaluator
// available in this build. The first standalone fixture uses the async
// lifecycle evaluator; other predicate kinds are intentionally reported as
// unsupported instead of being guessed.
func EvaluatePredicate(spec PredicateSpec, events []Event, probeOnly bool) OracleResult {
	if spec.Kind == PredicateOrdering || spec.Kind == PredicateInvariant {
		if spec.Version == "async-lifecycle-v1" || spec.Version == "" {
			return EvaluateAsyncLifecycle(events, probeOnly)
		}
	}
	return OracleResult{Verdict: VerdictInsufficient,
		Reason: fmt.Sprintf("no evaluator registered for predicate %s/%s", spec.Kind, spec.Version)}
}

// EvaluateAsyncLifecycle checks the first demo invariant: once cancel returns,
// the callback must have exited before the final release. Missing events are
// evidence gaps, not proof that the invariant was violated.
func EvaluateAsyncLifecycle(events []Event, probeOnly bool) OracleResult {
	if len(events) == 0 {
		return OracleResult{Verdict: VerdictInsufficient, Reason: "no LLMEXP events were observed"}
	}
	// The first fixture has one object generation. Keep unrelated events from
	// another object or generation from satisfying the predicate.
	type identity struct {
		object     string
		generation uint64
	}
	groups := make(map[identity][]Event)
	var order []identity
	for _, event := range events {
		id := identity{event.Object, event.Generation}
		if _, ok := groups[id]; !ok {
			order = append(order, id)
		}
		groups[id] = append(groups[id], event)
	}
	var filtered []Event
	for _, id := range order {
		candidate := groups[id]
		for _, event := range candidate {
			if event.Kind == "submit" {
				filtered = candidate
				break
			}
		}
		if len(filtered) > 0 {
			seen := make(map[string]bool)
			for _, event := range filtered {
				seen[event.Kind] = true
			}
			complete := true
			for _, required := range []string{"submit", "cancel_return", "callback_enter", "callback_exit", "release"} {
				complete = complete && seen[required]
			}
			if complete {
				break
			}
		}
	}
	if len(filtered) == 0 {
		return OracleResult{Verdict: VerdictInsufficient, Reason: "submit event has no object identity"}
	}
	seen := map[string]bool{}
	for _, event := range filtered {
		seen[event.Kind] = true
	}
	for _, required := range []string{"submit", "cancel_return", "callback_enter", "callback_exit", "release"} {
		if !seen[required] {
			return OracleResult{Verdict: VerdictInsufficient,
				Reason: fmt.Sprintf("required event %q was not observed", required)}
		}
	}
	lastCancel, callbackExit, release := -1, -1, -1
	for _, event := range filtered {
		switch event.Kind {
		case "cancel_return":
			lastCancel = event.Seq
		case "callback_exit":
			callbackExit = event.Seq
		case "release":
			release = event.Seq
		}
	}
	if lastCancel >= 0 && callbackExit >= 0 && release >= 0 && callbackExit > lastCancel && release < callbackExit {
		if probeOnly {
			return OracleResult{Verdict: VerdictObservedViolation,
				Reason: "release occurred before callback_exit; observed only with experimental probe"}
		}
		return OracleResult{Verdict: VerdictObservedViolation,
			Reason: "release occurred before callback_exit"}
	}
	return OracleResult{Verdict: VerdictFalsified, Reason: "observed lifecycle ordering satisfies the invariant"}
}
