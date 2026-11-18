// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

// Package experiment implements the first deterministic runtime-guided
// experiment workflow. The LLM-facing hypothesis loop can build on this data
// plane without introducing another VM execution service.
package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/syzkaller/pkg/aflow"
	"github.com/google/syzkaller/pkg/aflow/action/crash"
	"github.com/google/syzkaller/pkg/aflow/action/kernel"
	"github.com/google/syzkaller/pkg/aflow/ai"
	"github.com/google/syzkaller/pkg/aflow/experiment"
)

// Inputs describes one isolated semantic experiment. ProgramSyz is supplied
// by a seed/corpus or a future LLM generator; this workflow is responsible for
// executing and classifying its evidence.
type Inputs struct {
	AgentName         string
	KernelRepo        string
	KernelCommit      string
	KernelConfig      string
	Image             string
	Type              string
	VM                json.RawMessage
	Syzkaller         string
	SyzkallerCommit   string
	TargetOS          string
	TargetArch        string
	TargetVMArch      string `json:",omitempty"`
	Snapshot          bool
	Driver            string
	Probe             string `json:",omitempty"`
	ProgramSyz        string
	Hypothesis        string
	Falsification     string
	Scenario          string
	Perturbation      string `json:",omitempty"`
	IsNegativeControl bool
	ProbeOnly         bool
}

type runArgs struct {
	EnvironmentPrompt string `json:",omitempty"`
	AgentName         string
	KernelCommit      string
	KernelConfig      string
	KernelSrc         string
	KernelObj         string
	Image             string
	Type              string
	VM                json.RawMessage
	Syzkaller         string
	SyzkallerCommit   string
	TargetOS          string
	TargetArch        string
	TargetVMArch      string
	Snapshot          bool
	Driver            string
	Probe             string
	ProgramSyz        string
	Hypothesis        string
	Falsification     string
	Scenario          string
	Perturbation      string
	IsNegativeControl bool
	ProbeOnly         bool
}

type runResult struct {
	ManifestID        string
	ExecutionCachedID string
	ObservedEvents    []string `json:",omitempty"`
	Observation       string   `json:",omitempty"`
	OracleVerdict     string
	OracleReason      string
	NegativeControl   string `json:",omitempty"`
	Replays           int    `json:",omitempty"`
	Success           bool
	Reason            string `json:",omitempty"`
}

var run = aflow.NewFuncAction("run-runtime-experiment", runExperiment)

func init() {
	aflow.Register[Inputs, ai.RuntimeExperimentOutputs](
		ai.WorkflowRuntimeExperiment,
		"execute an isolated syz program and classify runtime evidence for an explicit semantic hypothesis",
		&aflow.Flow{
			Root: aflow.Pipeline(
				kernel.Checkout,
				kernel.Build,
				crash.ActionConfigureRunner,
				run,
			),
		},
	)
}

func runExperiment(ctx *aflow.Context, args runArgs) (runResult, error) {
	if strings.TrimSpace(args.ProgramSyz) == "" {
		return runResult{}, aflow.BadCallError("ProgramSyz is required")
	}
	if args.Driver == "" || args.Hypothesis == "" || args.Falsification == "" || args.Scenario == "" {
		return runResult{}, aflow.BadCallError("Driver, Hypothesis, Falsification and Scenario are required")
	}
	programHash := sha256.Sum256([]byte(args.ProgramSyz))
	manifest, err := experiment.NewManifest(experiment.Manifest{
		KernelCommit: args.KernelCommit, SyzkallerCommit: args.SyzkallerCommit,
		Driver: args.Driver, Probe: args.Probe, ProgramHash: hex.EncodeToString(programHash[:]),
		Hypothesis: args.Hypothesis, Falsification: args.Falsification,
		Scenario: args.Scenario, Perturbation: args.Perturbation,
		NegativeControl: args.IsNegativeControl,
	})
	if err != nil {
		return runResult{}, aflow.BadCallError("invalid experiment manifest: %v", err)
	}

	cachedID, err := crash.ExecuteSeedFunc(ctx, crash.ExecuteSeedArgs{
		TargetConfig: crash.TargetConfig{
			Target:    crash.Target{TargetOS: args.TargetOS, TargetArch: args.TargetArch, TargetVMArch: args.TargetVMArch},
			AgentName: args.AgentName, Syzkaller: args.Syzkaller, Image: args.Image, Type: args.Type,
			VM: args.VM, KernelSrc: args.KernelSrc, KernelObj: args.KernelObj, KernelCommit: args.KernelCommit,
			KernelConfig: args.KernelConfig, Snapshot: args.Snapshot,
		},
		ReproSyz: args.ProgramSyz,
	})
	if err != nil {
		return runResult{ManifestID: manifest.ID,
			OracleVerdict: string(experiment.VerdictInsufficient), NegativeControl: negativeControlResult(args.IsNegativeControl),
			Replays: 1, Reason: err.Error()}, nil
	}
	evidence, err := crash.LoadExecutionEvidence(ctx, cachedID)
	if err != nil {
		return runResult{}, fmt.Errorf("load execution evidence: %w", err)
	}
	events, warnings := experiment.ParseEvents(evidence.ConsoleOutput)
	oracle := experiment.EvaluateAsyncLifecycle(events, args.ProbeOnly)
	observed := make([]string, 0, len(events))
	for _, event := range events {
		observed = append(observed, event.Kind)
	}
	reason := oracle.Reason
	if len(warnings) != 0 {
		reason += "; event warnings: " + strings.Join(warnings, ", ")
	}
	if evidence.BugTitle != "" {
		reason += "; crash: " + evidence.BugTitle
	}
	return runResult{
		ManifestID: manifest.ID, ExecutionCachedID: cachedID,
		ObservedEvents: observed, Observation: strings.Join(observed, " -> "),
		OracleVerdict: string(oracle.Verdict), OracleReason: reason,
		NegativeControl: negativeControlResult(args.IsNegativeControl), Replays: 1,
		Success: oracle.Verdict == experiment.VerdictConfirmedOnOriginal,
		Reason:  reason,
	}, nil
}

func negativeControlResult(enabled bool) string {
	if enabled {
		return "pending-independent-replay"
	}
	return ""
}
