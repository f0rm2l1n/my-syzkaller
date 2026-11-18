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

// EvaluateAsyncLifecycle checks the first demo invariant: once cancel returns,
// the callback must have exited before the final release. Missing events are
// evidence gaps, not proof that the invariant was violated.
func EvaluateAsyncLifecycle(events []Event, probeOnly bool) OracleResult {
	if len(events) == 0 {
		return OracleResult{Verdict: VerdictInsufficient, Reason: "no LLMEXP events were observed"}
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.Kind] = true
	}
	for _, required := range []string{"submit", "cancel_return", "callback_enter", "callback_exit", "release"} {
		if !seen[required] {
			return OracleResult{Verdict: VerdictInsufficient,
				Reason: fmt.Sprintf("required event %q was not observed", required)}
		}
	}
	lastCancel, callbackExit, release := -1, -1, -1
	for _, event := range events {
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
