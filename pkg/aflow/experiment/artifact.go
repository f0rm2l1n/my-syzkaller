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
)

// PackageID derives a stable identity from the semantic proposal and manifest.
// Runtime evidence is deliberately excluded: coverage and call-error fields
// use interface values and may have a different concrete Go type after a
// JSON round trip. Replays update the same package identity and pointer.
func PackageID(p SemanticExperimentPackage) string {
	identity := struct {
		SchemaVersion   int             `json:"schema_version"`
		ManifestID      string          `json:"manifest_id"`
		Driver          string          `json:"driver"`
		Hypothesis      string          `json:"hypothesis"`
		Falsification   string          `json:"falsification"`
		Scenario        ScenarioSpec    `json:"scenario"`
		Predicate       PredicateSpec   `json:"predicate"`
		Observation     ObservationPlan `json:"observation_plan"`
		ExpectedVerdict *Verdict        `json:"expected_verdict,omitempty"`
	}{
		SchemaVersion: p.SchemaVersion, ManifestID: p.Manifest.ID, Driver: p.Driver,
		Hypothesis: p.Hypothesis, Falsification: p.Falsification, Scenario: p.Scenario,
		Predicate: p.Predicate, Observation: p.Observation, ExpectedVerdict: p.ExpectedVerdict,
	}
	data, _ := json.Marshal(identity)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:16])
}

// WritePackage atomically writes the canonical package JSON and returns its
// directory. Callers may place raw console and coverage files next to it.
func WritePackage(root string, p SemanticExperimentPackage) (string, error) {
	if p.SchemaVersion == 0 {
		p.SchemaVersion = SchemaVersion
	}
	if p.PackageID == "" {
		p.PackageID = PackageID(p)
	}
	if p.Manifest.ID == "" {
		return "", fmt.Errorf("package manifest has no ID")
	}
	dir := filepath.Join(root, p.PackageID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".package-*.json")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, "package.json")); err != nil {
		return "", err
	}
	return dir, nil
}
