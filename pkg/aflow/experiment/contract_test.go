// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package experiment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func testProposal() ExperimentProposal {
	return ExperimentProposal{
		Driver: "fixture", Hypothesis: "callback completes before release",
		Falsification: "release precedes callback exit",
		Scenario:      ScenarioSpec{ProgramSyz: "getpid()"},
		Predicate:     PredicateSpec{Kind: PredicateInvariant, Version: "async-lifecycle-v1", Expression: "callback_exit < release"},
		Observation:   ObservationPlan{EventFormat: "LLMEXP"},
	}
}

func TestProposalValidationAndStrictDecode(t *testing.T) {
	p := testProposal()
	if err := ValidateProposal(p, "linux", "amd64"); err != nil {
		t.Fatalf("valid proposal rejected: %v", err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeProposal(raw); err != nil {
		t.Fatalf("valid proposal failed to decode: %v", err)
	}
	if _, err := DecodeProposal(append(raw, []byte(`{"unexpected":true}`)...)); err == nil {
		t.Fatal("trailing JSON unexpectedly accepted")
	}
	unknown := append(raw[:len(raw)-1], []byte(`,"unknown":true}`)...)
	if _, err := DecodeProposal(unknown); err == nil {
		t.Fatal("unknown field unexpectedly accepted")
	}
	p.Predicate.Kind = "made-up"
	if err := ValidateProposal(p, "linux", "amd64"); err == nil {
		t.Fatal("unknown predicate unexpectedly accepted")
	}
}

func TestWritePackageIsReadableAndAtomic(t *testing.T) {
	manifest, err := NewManifest(Manifest{
		KernelCommit: "kernel", SyzkallerCommit: "syz", Driver: "fixture",
		ProgramHash: "program", Hypothesis: "h", Falsification: "f", Scenario: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	pkg := SemanticExperimentPackage{Manifest: manifest, Driver: "fixture", Status: "observed"}
	root := t.TempDir()
	dir, err := WritePackage(root, pkg)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got SemanticExperimentPackage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != SchemaVersion || got.PackageID == "" {
		t.Fatalf("unexpected package: %+v", got)
	}
}

func TestPackageIDSurvivesJSONRoundTripWithDynamicEvidence(t *testing.T) {
	manifest, err := NewManifest(Manifest{
		KernelCommit: "kernel", SyzkallerCommit: "syz", Driver: "fixture",
		ProgramHash: "program", Hypothesis: "h", Falsification: "f", Scenario: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	pkg := SemanticExperimentPackage{
		SchemaVersion: SchemaVersion, Manifest: manifest, Driver: "fixture",
		Hypothesis: "h", Falsification: "f",
		Scenario:    ScenarioSpec{ProgramSyz: "getpid()"},
		Predicate:   PredicateSpec{Kind: PredicateInvariant, Version: "v1", Expression: "x"},
		Observation: ObservationPlan{EventFormat: "LLMEXP"},
		Executions:  []ExecutionEvidence{{Coverage: [][]string{{"frame"}}, CallErrors: []map[string]any{{"errno": 1}}}},
	}
	id := PackageID(pkg)
	data, err := json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	var roundTripped SemanticExperimentPackage
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatal(err)
	}
	if got := PackageID(roundTripped); got != id {
		t.Fatalf("package ID changed after JSON round trip: before=%q after=%q", id, got)
	}
}
