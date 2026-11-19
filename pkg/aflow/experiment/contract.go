// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package experiment

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SchemaVersion is the on-disk semantic experiment package schema.
const SchemaVersion = 1

// PredicateKind identifies the semantic property evaluated by an experiment.
// New kinds must be registered with an evaluator before they can be executed.
type PredicateKind string

const (
	PredicateCoverage     PredicateKind = "coverage"
	PredicateState        PredicateKind = "state"
	PredicateSequence     PredicateKind = "sequence"
	PredicateOrdering     PredicateKind = "ordering"
	PredicateInvariant    PredicateKind = "invariant"
	PredicateDifferential PredicateKind = "differential"
	PredicateCrash        PredicateKind = "crash"
)

// PredicateSpec describes a property independently of a particular driver.
type PredicateSpec struct {
	Kind       PredicateKind     `json:"kind" jsonschema:"Predicate kind from the allowed semantic predicate set."`
	Version    string            `json:"version" jsonschema:"Version of the predicate evaluator."`
	Expression string            `json:"expression" jsonschema:"Falsifiable predicate expression."`
	Parameters map[string]string `json:"parameters,omitempty" jsonschema:"Predicate parameters."`
}

// ScenarioSpec is the executable part of an experiment proposal.
type ScenarioSpec struct {
	ProgramSyz    string   `json:"program_syz" jsonschema:"Executable syzkaller program without code fences."`
	Preconditions []string `json:"preconditions,omitempty" jsonschema:"Required runtime preconditions."`
	Perturbation  string   `json:"perturbation,omitempty" jsonschema:"Allowed scheduling or observation perturbation."`
}

// ObservationPlan limits how a scenario may collect evidence.
type ObservationPlan struct {
	Probe       string   `json:"probe,omitempty" jsonschema:"Allowed probe or fixture identity."`
	EventFormat string   `json:"event_format,omitempty" jsonschema:"Structured evidence format."`
	Required    []string `json:"required,omitempty" jsonschema:"Required observation kinds."`
}

// ExperimentProposal is the only semantic object the LLM is allowed to
// propose. Host-generated evidence and verdicts are deliberately absent.
type ExperimentProposal struct {
	Driver        string          `json:"driver" jsonschema:"Driver or fixture identity."`
	Hypothesis    string          `json:"hypothesis" jsonschema:"Semantic hypothesis to test."`
	Falsification string          `json:"falsification" jsonschema:"Evidence that would falsify the hypothesis."`
	Scenario      ScenarioSpec    `json:"scenario" jsonschema:"Executable semantic scenario."`
	Predicate     PredicateSpec   `json:"predicate" jsonschema:"Predicate evaluated by the host."`
	Observation   ObservationPlan `json:"observation_plan" jsonschema:"Observation plan constrained by the host."`
}

// ExecutionEvidence is a serializable view of one execution. It is kept
// separate from the aflow cache object so packages remain independently
// auditable.
type ExecutionEvidence struct {
	Attempt        int          `json:"attempt"`
	ExecutionID    string       `json:"execution_id,omitempty"`
	ProgramSyz     string       `json:"program_syz"`
	ObservedEvents []Event      `json:"observed_events,omitempty"`
	Warnings       []string     `json:"warnings,omitempty"`
	Coverage       any          `json:"coverage,omitempty"`
	CallErrors     any          `json:"call_errors,omitempty"`
	ConsoleFile    string       `json:"console_file,omitempty"`
	CrashTitle     string       `json:"crash_title,omitempty"`
	CrashReport    string       `json:"crash_report,omitempty"`
	ExecutionError string       `json:"execution_error,omitempty"`
	Oracle         OracleResult `json:"oracle"`
}

// ReplayStatus records independent attempts and their verdicts.
type ReplayStatus struct {
	Requested  int       `json:"requested"`
	Completed  int       `json:"completed"`
	Passed     int       `json:"passed"`
	Verdicts   []Verdict `json:"verdicts,omitempty"`
	PassedGate bool      `json:"passed_gate"`
}

// SemanticExperimentPackage is the canonical standalone artifact. Fields
// populated from executions are written by the host, never trusted from LLM
// output.
type SemanticExperimentPackage struct {
	SchemaVersion   int                 `json:"schema_version"`
	PackageID       string              `json:"package_id"`
	Manifest        Manifest            `json:"manifest"`
	Driver          string              `json:"driver"`
	Hypothesis      string              `json:"hypothesis"`
	Falsification   string              `json:"falsification"`
	Scenario        ScenarioSpec        `json:"scenario"`
	Predicate       PredicateSpec       `json:"predicate"`
	Observation     ObservationPlan     `json:"observation_plan"`
	Executions      []ExecutionEvidence `json:"executions,omitempty"`
	Replay          ReplayStatus        `json:"replay"`
	Status          string              `json:"status"`
	ExpectedVerdict *Verdict            `json:"expected_verdict,omitempty"`
	Provenance      map[string]string   `json:"provenance,omitempty"`
}

// ValidateProposal checks the host-side contract before any VM is started.
func ValidateProposal(p ExperimentProposal, targetOS, targetArch string) error {
	for name, value := range map[string]string{
		"driver": p.Driver, "hypothesis": p.Hypothesis, "falsification": p.Falsification,
		"program_syz": p.Scenario.ProgramSyz, "predicate_version": p.Predicate.Version,
		"predicate_expression": p.Predicate.Expression,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("proposal field %s is empty", name)
		}
	}
	if len(p.Hypothesis) > 16<<10 || len(p.Falsification) > 16<<10 || len(p.Scenario.ProgramSyz) > 1<<20 {
		return fmt.Errorf("proposal exceeds size limit")
	}
	switch p.Predicate.Kind {
	case PredicateCoverage, PredicateState, PredicateSequence, PredicateOrdering,
		PredicateInvariant, PredicateDifferential, PredicateCrash:
	default:
		return fmt.Errorf("unsupported predicate kind %q", p.Predicate.Kind)
	}
	if targetOS == "" || targetArch == "" {
		return fmt.Errorf("target OS and architecture are required")
	}
	return nil
}

// DecodeProposal is used by adapters that retain the raw proposal JSON. The
// standalone aflow tool receives a typed value after aflow's map conversion,
// so it relies on ValidateProposal for the executable contract.
func DecodeProposal(data []byte) (ExperimentProposal, error) {
	var p ExperimentProposal
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return ExperimentProposal{}, fmt.Errorf("decode proposal: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return ExperimentProposal{}, fmt.Errorf("proposal contains trailing JSON")
	}
	return p, nil
}
