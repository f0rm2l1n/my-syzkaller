// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/syzkaller/pkg/aflow"
	"github.com/google/syzkaller/pkg/aflow/action/crash"
	"github.com/google/syzkaller/pkg/aflow/action/kernel"
	"github.com/google/syzkaller/pkg/aflow/ai"
	exp "github.com/google/syzkaller/pkg/aflow/experiment"
	"github.com/google/syzkaller/pkg/hash"
)

const (
	defaultReplayCount         = 3
	maxStandaloneReplays       = 32
	defaultProposalAttempts    = 5
	maxStandaloneLLMIterations = 12
)

// StandaloneInputs are the explicit inputs for the local semantic experiment.
// The workflow has no dependency on a running syz-manager.
type StandaloneInputs struct {
	AgentName       string
	KernelRepo      string
	KernelCommit    string
	KernelConfig    string
	Image           string
	Type            string
	VM              json.RawMessage
	Syzkaller       string
	SyzkallerCommit string
	TargetOS        string
	TargetArch      string
	TargetVMArch    string `json:",omitempty"`
	Snapshot        bool

	Driver              string
	ProgramSyz          string `json:",omitempty"`
	RuntimeContext      string `json:",omitempty"`
	PredicateKind       string `json:",omitempty"`
	PredicateVersion    string `json:",omitempty"`
	PredicateExpression string `json:",omitempty"`
	ExpectedVerdict     string `json:",omitempty"`
	UseLLM              bool   `json:",omitempty"`
	ReplayCount         int    `json:",omitempty"`
	MaxProposalAttempts int    `json:",omitempty"`
	ArtifactRoot        string `json:",omitempty"`
}

type standaloneRunArgs struct {
	StandaloneInputs
	KernelSrc string
	KernelObj string
}

type standaloneMarker struct {
	StandaloneDone    bool
	StandaloneSummary string
}

type standalonePreflightResult struct{}

type proposalToolState struct {
	EnvironmentPrompt   string
	AgentName           string
	KernelCommit        string
	KernelConfig        string
	KernelSrc           string
	KernelObj           string
	Image               string
	Type                string
	VM                  json.RawMessage
	Syzkaller           string
	SyzkallerCommit     string
	TargetOS            string
	TargetArch          string
	TargetVMArch        string
	Snapshot            bool
	RuntimeContext      string
	ArtifactRoot        string
	ReplayCount         int
	MaxProposalAttempts int
	ExpectedVerdict     string
}

type proposalToolArgs struct {
	Proposal exp.ExperimentProposal `json:"proposal" jsonschema:"Complete semantic experiment proposal."`
}

type proposalToolResult struct {
	PackageID     string      `json:"package_id,omitempty" jsonschema:"Host-generated package identity."`
	ExecutionID   string      `json:"execution_id,omitempty" jsonschema:"Host execution identity."`
	Verdict       exp.Verdict `json:"verdict" jsonschema:"Host-evaluated oracle verdict."`
	ObservedKinds []string    `json:"observed_kinds,omitempty" jsonschema:"Observed event kinds."`
	Reason        string      `json:"reason" jsonschema:"Host evidence summary."`
}

type agentAssessment struct {
	Done    bool   `jsonschema:"Set true when a proposal has enough evidence or the experiment is blocked."`
	Summary string `jsonschema:"Short evidence-based summary. Do not claim fields not returned by tools."`
}

type standaloneFinalizeArgs struct {
	StandaloneInputs
	EnvironmentPrompt string
	KernelSrc         string
	KernelObj         string
	StandaloneDone    bool
	StandaloneSummary string
}

var standaloneDeterministic = aflow.NewFuncAction("standalone-deterministic-experiment", runStandaloneDeterministic)
var standalonePreflight = aflow.NewFuncAction("standalone-preflight", func(_ *aflow.Context, args StandaloneInputs) (standalonePreflightResult, error) {
	return standalonePreflightResult{}, validateStandaloneInputs(args)
})
var standaloneFinalize = aflow.NewFuncAction("standalone-finalize", finalizeStandalone)

func validateStandaloneInputs(args StandaloneInputs) error {
	if args.ReplayCount < 0 {
		return aflow.BadCallError("ReplayCount cannot be negative")
	}
	count := args.ReplayCount
	if count == 0 {
		count = defaultReplayCount
	}
	if count > maxStandaloneReplays {
		return aflow.BadCallError("ReplayCount=%d exceeds maximum %d", count, maxStandaloneReplays)
	}
	if count > 1 && !args.Snapshot {
		return aflow.BadCallError("ReplayCount=%d requires Snapshot=true for independent VM replay", count)
	}
	if args.ExpectedVerdict != "" {
		switch exp.Verdict(args.ExpectedVerdict) {
		case exp.VerdictFalsified, exp.VerdictInsufficient, exp.VerdictObservedViolation, exp.VerdictConfirmedOnOriginal:
		default:
			return aflow.BadCallError("unsupported ExpectedVerdict %q", args.ExpectedVerdict)
		}
	}
	return nil
}

var standaloneProposalTool = aflow.NewFuncTool(
	"submit-experiment-proposal",
	func(ctx *aflow.Context, state proposalToolState, args proposalToolArgs) (proposalToolResult, error) {
		if err := exp.ValidateProposal(args.Proposal, state.TargetOS, state.TargetArch); err != nil {
			return proposalToolResult{}, aflow.BadCallError("proposal rejected: %v", err)
		}
		limit := state.MaxProposalAttempts
		if limit <= 0 {
			limit = defaultProposalAttempts
		}
		if attempts := countProposalAttempts(artifactRoot(ctx, state.ArtifactRoot)); attempts >= limit {
			return proposalToolResult{}, aflow.BadCallError("proposal attempt limit %d reached", limit)
		}
		res, err := executeProposal(ctx, state.StandaloneInputs(), state.KernelSrc, state.KernelObj, args.Proposal, 1)
		if err != nil {
			return proposalToolResult{Reason: err.Error(), Verdict: exp.VerdictInsufficient}, nil
		}
		return proposalToolResult{
			PackageID: res.Package.PackageID, ExecutionID: res.Execution.ExecutionID,
			Verdict: res.Execution.Oracle.Verdict, ObservedKinds: eventKinds(res.Execution.ObservedEvents),
			Reason: res.Execution.Oracle.Reason,
		}, nil
	},
	"Validate and execute one semantic experiment proposal in the isolated Linux VM, then return host-observed evidence.")

func (s proposalToolState) StandaloneInputs() StandaloneInputs {
	return StandaloneInputs{
		AgentName: s.AgentName, KernelCommit: s.KernelCommit, KernelConfig: s.KernelConfig,
		Image: s.Image, Type: s.Type, VM: s.VM, Syzkaller: s.Syzkaller,
		SyzkallerCommit: s.SyzkallerCommit, TargetOS: s.TargetOS, TargetArch: s.TargetArch,
		TargetVMArch: s.TargetVMArch, Snapshot: s.Snapshot, RuntimeContext: s.RuntimeContext,
		ArtifactRoot: s.ArtifactRoot, ReplayCount: s.ReplayCount,
		MaxProposalAttempts: s.MaxProposalAttempts,
		ExpectedVerdict:     s.ExpectedVerdict,
	}
}

var standaloneAgent = &aflow.LLMAgent{
	Name:  "semantic-experiment-agent",
	Model: aflow.CoreModel,
	Tools: aflow.Tools(standaloneProposalTool),
	Outputs: aflow.ValidatedLLMOutputs[agentAssessment](func(_ *aflow.Context, _ struct{}, out agentAssessment) (agentAssessment, error) {
		if strings.TrimSpace(out.Summary) == "" {
			return out, aflow.BadCallError("Summary must explain the evidence or blocker")
		}
		return out, nil
	}),
	MaxIterations: maxStandaloneLLMIterations,
	Instruction: `You design one semantic syzkaller experiment from the supplied runtime context.
Only use submit-experiment-proposal for executable work. The host validates every field and supplies the
actual verdict, events and execution identity. Do not invent evidence, verdicts, coverage or crash reports.
You may revise a proposal after tool feedback. Finish with set-results and summarize only observed facts.`,
	Prompt: `Environment:
{{.EnvironmentPrompt}}

Runtime context:
{{.RuntimeContext}}

Target: {{.TargetOS}}/{{.TargetArch}}
The experiment must be executable with the available VM and must state a falsifiable predicate.
Propose a minimal scenario first.`,
}

func init() {
	// This workflow is intentionally separate from the original deterministic
	// data-plane entry. It can replace that entry for local standalone use while
	// retaining the same RunnerManager and execution actions.
	af := &aflow.Flow{
		Root: aflow.Pipeline(
			standalonePreflight,
			kernel.Checkout,
			kernel.BuildKeepArtifacts,
			crash.ActionConfigureRunner,
			&aflow.If{
				Condition: "UseLLM",
				Do: aflow.Pipeline(standaloneAgent, aflow.NewFuncAction("standalone-llm-marker", func(_ *aflow.Context, args agentAssessment) (standaloneMarker, error) {
					return standaloneMarker{StandaloneDone: args.Done, StandaloneSummary: args.Summary}, nil
				})),
				Else: standaloneDeterministic,
			},
			standaloneFinalize,
		),
	}
	afloWRegister(af)
}

func afloWRegister(flow *aflow.Flow) {
	aflow.Register[StandaloneInputs, ai.RuntimeStandaloneOutputs](
		ai.WorkflowRuntimeStandalone,
		"run a standalone semantic experiment and produce an auditable package",
		flow,
	)
}

func runStandaloneDeterministic(ctx *aflow.Context, args standaloneRunArgs) (standaloneMarker, error) {
	if strings.TrimSpace(args.ProgramSyz) == "" {
		return standaloneMarker{}, aflow.BadCallError("ProgramSyz is required when UseLLM is false")
	}
	proposal := proposalFromInputs(args.StandaloneInputs)
	if err := exp.ValidateProposal(proposal, args.TargetOS, args.TargetArch); err != nil {
		return standaloneMarker{}, aflow.BadCallError("invalid deterministic proposal: %v", err)
	}
	_, err := executeProposal(ctx, args.StandaloneInputs, args.KernelSrc, args.KernelObj, proposal, 1)
	return standaloneMarker{StandaloneDone: err == nil, StandaloneSummary: "deterministic proposal executed"}, err
}

func finalizeStandalone(ctx *aflow.Context, args standaloneFinalizeArgs) (ai.RuntimeStandaloneOutputs, error) {
	root := artifactRoot(ctx, args.ArtifactRoot)
	latest, err := os.ReadFile(filepath.Join(root, "latest.json"))
	if err != nil {
		return ai.RuntimeStandaloneOutputs{Status: "no-artifact", Reason: err.Error()}, nil
	}
	var pointer struct {
		PackageDir string `json:"package_dir"`
		PackageID  string `json:"package_id"`
	}
	if err := json.Unmarshal(latest, &pointer); err != nil {
		return ai.RuntimeStandaloneOutputs{Status: "invalid-artifact-pointer", Reason: err.Error()}, nil
	}
	if err := validateArtifactPointer(root, pointer.PackageDir, pointer.PackageID); err != nil {
		return ai.RuntimeStandaloneOutputs{PackageID: pointer.PackageID, Status: "invalid-artifact-pointer", Reason: err.Error()}, nil
	}
	data, err := os.ReadFile(filepath.Join(pointer.PackageDir, "package.json"))
	if err != nil {
		return ai.RuntimeStandaloneOutputs{PackageID: pointer.PackageID, Status: "missing-artifact", Reason: err.Error()}, nil
	}
	var pkg exp.SemanticExperimentPackage
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ai.RuntimeStandaloneOutputs{PackageID: pointer.PackageID, Status: "invalid-artifact", Reason: err.Error()}, nil
	}
	if pkg.PackageID != pointer.PackageID || exp.PackageID(pkg) != pointer.PackageID {
		return ai.RuntimeStandaloneOutputs{PackageID: pointer.PackageID, Status: "invalid-artifact", Reason: "package identity does not match latest pointer or package contents"}, nil
	}
	replayCount := args.ReplayCount
	if replayCount <= 0 {
		replayCount = defaultReplayCount
	}
	// Replay is deliberately host-controlled and runs after either the
	// deterministic proposal or the bounded LLM loop has selected a package.
	proposal := proposalFromPackage(pkg)
	status, replayEvidence, replayErr := replayProposal(ctx, args.StandaloneInputs, args.KernelSrc, args.KernelObj, proposal, replayCount)
	pkg.Replay = status
	pkg.Executions = append(pkg.Executions, replayEvidence...)
	if replayErr != nil {
		pkg.Status = "replay-error"
	} else if status.PassedGate {
		pkg.Status = "replay-passed"
	} else {
		pkg.Status = "replay-failed"
	}
	pkg.PackageID = exp.PackageID(pkg)
	finalDir, writeErr := persistPackage(root, pkg)
	if writeErr != nil {
		return ai.RuntimeStandaloneOutputs{
			PackageID: pkg.PackageID, PackageDir: pointer.PackageDir, Status: "artifact-error",
			OracleVerdict: string(lastVerdict(pkg)), ReplayPassed: pkg.Replay.PassedGate,
			CompletedReplays: pkg.Replay.Completed, ProposalCount: len(pkg.Executions), Reason: writeErr.Error(),
		}, nil
	}
	return ai.RuntimeStandaloneOutputs{
		PackageID: pkg.PackageID, PackageDir: finalDir, Status: pkg.Status,
		OracleVerdict: string(lastVerdict(pkg)), ReplayPassed: pkg.Replay.PassedGate,
		CompletedReplays: pkg.Replay.Completed, ProposalCount: len(pkg.Executions),
	}, nil
}

type proposalExecution struct {
	Package   exp.SemanticExperimentPackage
	Execution exp.ExecutionEvidence
}

func executeProposal(ctx *aflow.Context, args StandaloneInputs, kernelSrc, kernelObj string,
	proposal exp.ExperimentProposal, attempt int) (proposalExecution, error) {
	root := artifactRoot(ctx, args.ArtifactRoot)
	programHash := sha256.Sum256([]byte(proposal.Scenario.ProgramSyz))
	configData := []byte(args.KernelConfig)
	if kernelConfig, readErr := os.ReadFile(filepath.Join(kernelObj, ".config")); readErr == nil {
		configData = kernelConfig
	}
	configHash := sha256.Sum256(configData)
	imageHash := ""
	if sig, err := hash.File(args.Image); err == nil {
		imageHash = sig.String()
	}
	manifest, err := exp.NewManifest(exp.Manifest{
		KernelCommit: args.KernelCommit, KernelConfigHash: hex.EncodeToString(configHash[:]),
		ImageHash: imageHash, SyzkallerCommit: args.SyzkallerCommit, Driver: proposal.Driver,
		Probe: proposal.Observation.Probe, ProgramHash: hex.EncodeToString(programHash[:]),
		Hypothesis: proposal.Hypothesis, Falsification: proposal.Falsification,
		Scenario: proposal.Scenario.ProgramSyz, Perturbation: proposal.Scenario.Perturbation,
	})
	if err != nil {
		return proposalExecution{}, err
	}
	cachedID, execErr := crash.ExecuteSeedFuncWithEvidence(ctx, crash.ExecuteSeedArgs{
		TargetConfig: crash.TargetConfig{
			Target:    crash.Target{TargetOS: args.TargetOS, TargetArch: args.TargetArch, TargetVMArch: args.TargetVMArch},
			AgentName: args.AgentName, Syzkaller: args.Syzkaller, Image: args.Image, Type: args.Type,
			VM: args.VM, KernelSrc: kernelSrc, KernelObj: kernelObj, KernelCommit: args.KernelCommit,
			KernelConfig: args.KernelConfig, Snapshot: args.Snapshot,
		},
		ReproSyz:  proposal.Scenario.ProgramSyz,
		CacheSalt: fmt.Sprintf("semantic-attempt-%d", attempt),
	})
	// The RunnerManager was configured by the workflow action. The target paths
	// are injected below by the action state through the context's config.
	if execErr != nil && cachedID == "" {
		return proposalExecution{}, execErr
	}
	var evidence crash.ExecutionEvidence
	if cachedID != "" {
		evidence, err = crash.LoadExecutionEvidence(ctx, cachedID)
		if err != nil {
			return proposalExecution{}, err
		}
	}
	events, warnings := exp.ParseEvents(evidence.ConsoleOutput)
	oracle := exp.EvaluatePredicate(proposal.Predicate, events, false)
	if execErr != nil {
		warnings = append(warnings, execErr.Error())
	}
	execution := exp.ExecutionEvidence{
		Attempt: attempt, ExecutionID: cachedID, ProgramSyz: proposal.Scenario.ProgramSyz,
		ObservedEvents: events, Warnings: warnings, Coverage: evidence.Coverage,
		CallErrors: evidence.CallErrors, CrashTitle: evidence.BugTitle, CrashReport: evidence.Report,
		ExecutionError: evidence.Error, Oracle: oracle,
	}
	pkg := exp.SemanticExperimentPackage{
		SchemaVersion: exp.SchemaVersion, Manifest: manifest, Driver: proposal.Driver,
		Hypothesis: proposal.Hypothesis, Falsification: proposal.Falsification,
		Scenario: proposal.Scenario, Predicate: proposal.Predicate, Observation: proposal.Observation,
		Executions: []exp.ExecutionEvidence{execution}, Replay: exp.ReplayStatus{}, Status: "observed",
		Provenance: map[string]string{"controller": "syz-aflow-standalone"},
	}
	if args.ExpectedVerdict != "" {
		v := exp.Verdict(args.ExpectedVerdict)
		pkg.ExpectedVerdict = &v
	}
	if evidence.ConsoleOutput != "" {
		consoleName := fmt.Sprintf("console-%03d.log", attempt)
		execution.ConsoleFile = consoleName
		pkg.Executions[0] = execution
	}
	pkg.PackageID = exp.PackageID(pkg)
	if evidence.ConsoleOutput != "" {
		dir := filepath.Join(root, pkg.PackageID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return proposalExecution{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, execution.ConsoleFile), []byte(evidence.ConsoleOutput), 0o644); err != nil {
			return proposalExecution{}, err
		}
	}
	_, writeErr := persistPackage(root, pkg)
	if writeErr != nil {
		return proposalExecution{}, writeErr
	}
	if execErr != nil {
		return proposalExecution{Package: pkg, Execution: execution}, nil
	}
	return proposalExecution{Package: pkg, Execution: execution}, nil
}

// persistPackage writes the package and advances the latest pointer only after
// the package JSON is durable. Keeping this in one helper prevents replay from
// returning the pre-replay directory or leaving latest.json stale.
func persistPackage(root string, pkg exp.SemanticExperimentPackage) (string, error) {
	pkg.PackageID = exp.PackageID(pkg)
	dir, err := exp.WritePackage(root, pkg)
	if err != nil {
		return "", err
	}
	copyConsoleFiles(root, dir, pkg)
	if err := writeJSON(filepath.Join(root, "latest.json"), map[string]string{
		"package_dir": dir,
		"package_id":  pkg.PackageID,
	}); err != nil {
		return "", err
	}
	return dir, nil
}

func replayProposal(ctx *aflow.Context, args StandaloneInputs, kernelSrc, kernelObj string,
	proposal exp.ExperimentProposal, count int) (exp.ReplayStatus, []exp.ExecutionEvidence, error) {
	status := exp.ReplayStatus{Requested: count}
	if count > maxStandaloneReplays {
		return status, nil, fmt.Errorf("ReplayCount=%d exceeds maximum %d", count, maxStandaloneReplays)
	}
	if count > 1 && !args.Snapshot {
		return status, nil, fmt.Errorf("ReplayCount=%d requires Snapshot=true for independent VM replay", count)
	}
	var evidence []exp.ExecutionEvidence
	for i := 0; i < count; i++ {
		res, err := executeProposal(ctx, args, kernelSrc, kernelObj, proposal, i+2)
		if err != nil {
			return status, evidence, err
		}
		evidence = append(evidence, res.Execution)
		status.Completed++
		status.Verdicts = append(status.Verdicts, res.Execution.Oracle.Verdict)
	}
	status.Passed, status.PassedGate = replayGate(status.Verdicts, args.ExpectedVerdict)
	return status, evidence, nil
}

func replayGate(verdicts []exp.Verdict, expected string) (int, bool) {
	if len(verdicts) == 0 {
		return 0, false
	}
	if expected != "" {
		passed := 0
		for _, verdict := range verdicts {
			if string(verdict) == expected {
				passed++
			}
		}
		return passed, passed == len(verdicts)
	}
	first := verdicts[0]
	if first == exp.VerdictInsufficient {
		return 0, false
	}
	for _, verdict := range verdicts[1:] {
		if verdict != first {
			return 0, false
		}
	}
	return len(verdicts), true
}

func proposalFromInputs(args StandaloneInputs) exp.ExperimentProposal {
	kind := exp.PredicateKind(args.PredicateKind)
	if kind == "" {
		kind = exp.PredicateInvariant
	}
	version := args.PredicateVersion
	if version == "" {
		version = "async-lifecycle-v1"
	}
	expression := args.PredicateExpression
	if expression == "" {
		expression = "callback_exit occurs before release"
	}
	return exp.ExperimentProposal{
		Driver: args.Driver, Hypothesis: "the scenario exercises the requested semantic predicate",
		Falsification: "the observed evidence violates the predicate or is insufficient",
		Scenario:      exp.ScenarioSpec{ProgramSyz: args.ProgramSyz},
		Predicate:     exp.PredicateSpec{Kind: kind, Version: version, Expression: expression},
		Observation:   exp.ObservationPlan{Probe: args.Driver, EventFormat: "LLMEXP"},
	}
}

func proposalFromPackage(pkg exp.SemanticExperimentPackage) exp.ExperimentProposal {
	return exp.ExperimentProposal{Driver: pkg.Driver, Hypothesis: pkg.Hypothesis, Falsification: pkg.Falsification,
		Scenario: pkg.Scenario, Predicate: pkg.Predicate, Observation: pkg.Observation}
}

func artifactRoot(ctx *aflow.Context, root string) string {
	if root == "" {
		return filepath.Join(ctx.Workdir, "semantic-experiments")
	}
	if filepath.IsAbs(root) {
		return root
	}
	return filepath.Join(ctx.Workdir, root)
}

func validateArtifactPointer(root, packageDir, packageID string) error {
	if strings.TrimSpace(packageDir) == "" || strings.TrimSpace(packageID) == "" {
		return fmt.Errorf("artifact pointer is incomplete")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve artifact root: %w", err)
	}
	dir, err := filepath.Abs(packageDir)
	if err != nil {
		return fmt.Errorf("resolve package directory: %w", err)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("package directory %q is outside artifact root", packageDir)
	}
	if filepath.Base(dir) != packageID {
		return fmt.Errorf("package directory %q does not match package ID %q", packageDir, packageID)
	}
	return nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".json-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func countProposalAttempts(root string) int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			if _, err := os.Stat(filepath.Join(root, entry.Name(), "package.json")); err == nil {
				count++
			}
		}
	}
	return count
}

func copyConsoleFiles(root, destination string, pkg exp.SemanticExperimentPackage) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, execution := range pkg.Executions {
		if execution.ConsoleFile == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(destination, execution.ConsoleFile)); err == nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || entry.Name() == filepath.Base(destination) {
				continue
			}
			src := filepath.Join(root, entry.Name(), execution.ConsoleFile)
			data, err := os.ReadFile(src)
			if err != nil {
				continue
			}
			_ = os.WriteFile(filepath.Join(destination, execution.ConsoleFile), data, 0o644)
			break
		}
	}
}

func eventKinds(events []exp.Event) []string {
	res := make([]string, 0, len(events))
	for _, event := range events {
		res = append(res, event.Kind)
	}
	return res
}

func lastVerdict(pkg exp.SemanticExperimentPackage) exp.Verdict {
	if len(pkg.Replay.Verdicts) > 0 {
		return pkg.Replay.Verdicts[len(pkg.Replay.Verdicts)-1]
	}
	if len(pkg.Executions) > 0 {
		return pkg.Executions[len(pkg.Executions)-1].Oracle.Verdict
	}
	return exp.VerdictInsufficient
}
