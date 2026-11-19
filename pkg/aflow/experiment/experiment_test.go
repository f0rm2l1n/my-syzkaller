// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package experiment

import "testing"

func TestParseEventsAndOracle(t *testing.T) {
	output := "noise\n" +
		"[  12.345678][ T7] LLMEXP seq=1 object=ctx generation=1 cpu=0 task=7 kind=submit state=queued result=0\n" +
		"[  12.345679][ T8] LLMEXP seq=2 object=ctx generation=1 cpu=1 task=8 kind=callback_enter state=running result=0\n" +
		"[  12.345680][ T8] LLMEXP seq=3 object=ctx generation=1 cpu=1 task=8 kind=callback_exit state=done result=0\n" +
		"[  12.345681][ T7] LLMEXP seq=4 object=ctx generation=1 cpu=0 task=7 kind=cancel_return state=cancelled result=0\n" +
		"[  12.345682][ T7] LLMEXP seq=5 object=ctx generation=1 cpu=0 task=7 kind=release state=freed result=0\n"
	events, warnings := ParseEvents(output)
	if len(warnings) != 0 || len(events) != 5 {
		t.Fatalf("events=%v warnings=%v", events, warnings)
	}
	got := EvaluateAsyncLifecycle(events, false)
	if got.Verdict != VerdictFalsified {
		t.Fatalf("unexpected verdict: %+v", got)
	}
}

func TestOracleDoesNotTreatMissingEventsAsViolation(t *testing.T) {
	got := EvaluateAsyncLifecycle([]Event{{Seq: 1, Object: "ctx", Generation: 1, Kind: "submit"}}, true)
	if got.Verdict != VerdictInsufficient {
		t.Fatalf("unexpected verdict: %+v", got)
	}
}

func TestOracleMarksProbeOnlyOrderingViolation(t *testing.T) {
	events := []Event{
		{Seq: 1, Object: "ctx", Generation: 1, Kind: "submit"},
		{Seq: 2, Object: "ctx", Generation: 1, Kind: "cancel_return"},
		{Seq: 3, Object: "ctx", Generation: 1, Kind: "release"},
		{Seq: 4, Object: "ctx", Generation: 1, Kind: "callback_enter"},
		{Seq: 5, Object: "ctx", Generation: 1, Kind: "callback_exit"},
	}
	got := EvaluateAsyncLifecycle(events, true)
	if got.Verdict != VerdictObservedViolation {
		t.Fatalf("unexpected verdict: %+v", got)
	}
}

func TestOracleIgnoresOtherObjectGenerations(t *testing.T) {
	events := []Event{
		{Seq: 1, Object: "other", Generation: 9, Kind: "submit"},
		{Seq: 2, Object: "other", Generation: 9, Kind: "cancel_return"},
		{Seq: 3, Object: "other", Generation: 9, Kind: "release"},
		{Seq: 4, Object: "ctx", Generation: 1, Kind: "submit"},
		{Seq: 5, Object: "ctx", Generation: 1, Kind: "callback_enter"},
		{Seq: 6, Object: "ctx", Generation: 1, Kind: "callback_exit"},
		{Seq: 7, Object: "ctx", Generation: 1, Kind: "cancel_return"},
		{Seq: 8, Object: "ctx", Generation: 1, Kind: "release"},
	}
	got := EvaluatePredicate(PredicateSpec{Kind: PredicateInvariant, Version: "async-lifecycle-v1"}, events, false)
	if got.Verdict != VerdictFalsified {
		t.Fatalf("unexpected verdict: %+v", got)
	}
}

func TestManifestValidationAndStableID(t *testing.T) {
	manifest, err := NewManifest(Manifest{
		KernelCommit: "kernel", SyzkallerCommit: "syz", Driver: "llm_async_demo",
		ProgramHash: "program", Hypothesis: "h", Falsification: "f", Scenario: "s",
	})
	if err != nil || manifest.ID == "" {
		t.Fatalf("manifest=%+v err=%v", manifest, err)
	}
	if err := (Manifest{}).Validate(); err == nil {
		t.Fatal("empty manifest unexpectedly validated")
	}
}
