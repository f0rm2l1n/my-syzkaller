// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package experiment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	exp "github.com/google/syzkaller/pkg/aflow/experiment"
)

func standaloneTestPackage(t *testing.T) exp.SemanticExperimentPackage {
	t.Helper()
	manifest, err := exp.NewManifest(exp.Manifest{
		KernelCommit: "kernel", SyzkallerCommit: "syz", Driver: "fixture",
		ProgramHash: "program", Hypothesis: "h", Falsification: "f", Scenario: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	return exp.SemanticExperimentPackage{
		Manifest: manifest, Driver: "fixture", Hypothesis: "h", Falsification: "f",
		Scenario:  exp.ScenarioSpec{ProgramSyz: "getpid()"},
		Predicate: exp.PredicateSpec{Kind: exp.PredicateInvariant, Version: "async-lifecycle-v1", Expression: "callback_exit < release"},
		Status:    "observed",
	}
}

func TestPersistPackageAdvancesLatestPointer(t *testing.T) {
	root := t.TempDir()
	pkg := standaloneTestPackage(t)
	firstDir, err := persistPackage(root, pkg)
	if err != nil {
		t.Fatal(err)
	}

	pkg.Executions = []exp.ExecutionEvidence{{
		Attempt: 1, ProgramSyz: "getpid()",
		Oracle: exp.OracleResult{Verdict: exp.VerdictFalsified, Reason: "ordered"},
	}}
	secondDir, err := persistPackage(root, pkg)
	if err != nil {
		t.Fatal(err)
	}
	if firstDir != secondDir {
		t.Fatalf("package identity changed after adding execution evidence: first=%q second=%q", firstDir, secondDir)
	}

	data, err := os.ReadFile(filepath.Join(root, "latest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pointer struct {
		PackageDir string `json:"package_dir"`
		PackageID  string `json:"package_id"`
	}
	if err := json.Unmarshal(data, &pointer); err != nil {
		t.Fatal(err)
	}
	if pointer.PackageDir != secondDir || pointer.PackageID != filepath.Base(secondDir) {
		t.Fatalf("latest pointer = %+v, want dir %q", pointer, secondDir)
	}
	if _, err := os.Stat(filepath.Join(secondDir, "package.json")); err != nil {
		t.Fatalf("final package is missing: %v", err)
	}
}

func TestReplayRequiresSnapshotForMultipleAttempts(t *testing.T) {
	status, evidence, err := replayProposal(nil, StandaloneInputs{}, "", "",
		exp.ExperimentProposal{}, 2)
	if err == nil || !strings.Contains(err.Error(), "requires Snapshot=true") {
		t.Fatalf("replay error = %v, want snapshot guard", err)
	}
	if status.Requested != 2 || len(evidence) != 0 {
		t.Fatalf("replay started despite missing snapshot: status=%+v evidence=%v", status, evidence)
	}
}

func TestStandalonePreflightChecksEffectiveReplayCount(t *testing.T) {
	if err := validateStandaloneInputs(StandaloneInputs{}); err == nil || !strings.Contains(err.Error(), "requires Snapshot=true") {
		t.Fatalf("default replay was not guarded: %v", err)
	}
	if err := validateStandaloneInputs(StandaloneInputs{Snapshot: true, ReplayCount: -1}); err == nil || !strings.Contains(err.Error(), "cannot be negative") {
		t.Fatalf("negative replay count was accepted: %v", err)
	}
	if err := validateStandaloneInputs(StandaloneInputs{Snapshot: true, ReplayCount: 1, ExpectedVerdict: "unknown"}); err == nil || !strings.Contains(err.Error(), "unsupported ExpectedVerdict") {
		t.Fatalf("unknown expected verdict was accepted: %v", err)
	}
	if err := validateStandaloneInputs(StandaloneInputs{Snapshot: true, ReplayCount: 1, ExpectedVerdict: string(exp.VerdictFalsified)}); err != nil {
		t.Fatalf("valid preflight rejected: %v", err)
	}
}

func TestReplayGateRequiresStableDeterminateEvidence(t *testing.T) {
	passed, gate := replayGate([]exp.Verdict{exp.VerdictFalsified}, "")
	if passed != 1 || !gate {
		t.Fatalf("single determinate replay = (%d, %v), want (1, true)", passed, gate)
	}
	passed, gate = replayGate([]exp.Verdict{exp.VerdictFalsified, exp.VerdictFalsified}, "")
	if passed != 2 || !gate {
		t.Fatalf("stable replay = (%d, %v), want (2, true)", passed, gate)
	}
	passed, gate = replayGate([]exp.Verdict{exp.VerdictFalsified, exp.VerdictObservedViolation}, "")
	if passed != 0 || gate {
		t.Fatalf("unstable replay = (%d, %v), want (0, false)", passed, gate)
	}
	passed, gate = replayGate([]exp.Verdict{exp.VerdictFalsified, exp.VerdictFalsified}, string(exp.VerdictFalsified))
	if passed != 2 || !gate {
		t.Fatalf("expected verdict replay = (%d, %v), want (2, true)", passed, gate)
	}
}

func TestArtifactPointerMustStayInsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := validateArtifactPointer(root, filepath.Join(root, "id"), "id"); err != nil {
		t.Fatalf("valid pointer rejected: %v", err)
	}
	for _, tc := range []struct {
		dir string
		id  string
	}{
		{dir: filepath.Join(root, "..", "outside"), id: "outside"},
		{dir: filepath.Join(root, "id"), id: "other"},
		{dir: root, id: filepath.Base(root)},
	} {
		if err := validateArtifactPointer(root, tc.dir, tc.id); err == nil {
			t.Fatalf("invalid pointer accepted: dir=%q id=%q", tc.dir, tc.id)
		}
	}
}

func TestProposalDefaultsUseAsyncLifecycleEvaluator(t *testing.T) {
	proposal := proposalFromInputs(StandaloneInputs{
		Driver: "llm_async_demo", ProgramSyz: "getpid()",
	})
	if proposal.Predicate.Kind != exp.PredicateInvariant || proposal.Predicate.Version != "async-lifecycle-v1" {
		t.Fatalf("unexpected predicate defaults: %+v", proposal.Predicate)
	}
	if proposal.Predicate.Expression == "" || proposal.Hypothesis == "" || proposal.Falsification == "" {
		t.Fatalf("proposal defaults are incomplete: %+v", proposal)
	}
}
