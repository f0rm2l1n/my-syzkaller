// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

// Package experiment contains the small, deterministic data plane used by
// runtime-guided semantic experiments.  It deliberately does not make any
// claim about a kernel bug: it records evidence and applies an explicit oracle.
package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Manifest identifies every input which can change an experiment result.
type Manifest struct {
	ID               string    `json:"id"`
	CreatedAt        time.Time `json:"created_at"`
	KernelCommit     string    `json:"kernel_commit"`
	KernelConfigHash string    `json:"kernel_config_hash"`
	ImageHash        string    `json:"image_hash"`
	SyzkallerCommit  string    `json:"syzkaller_commit"`
	Driver           string    `json:"driver"`
	Probe            string    `json:"probe,omitempty"`
	ProgramHash      string    `json:"program_hash"`
	Hypothesis       string    `json:"hypothesis"`
	Falsification    string    `json:"falsification"`
	Scenario         string    `json:"scenario"`
	Perturbation     string    `json:"perturbation,omitempty"`
	NegativeControl  bool      `json:"negative_control"`
}

// Validate rejects incomplete manifests before a VM is started.
func (m Manifest) Validate() error {
	for name, value := range map[string]string{
		"kernel_commit":    m.KernelCommit,
		"syzkaller_commit": m.SyzkallerCommit,
		"driver":           m.Driver,
		"program_hash":     m.ProgramHash,
		"hypothesis":       m.Hypothesis,
		"falsification":    m.Falsification,
		"scenario":         m.Scenario,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("manifest field %s is empty", name)
		}
	}
	return nil
}

// NewManifest creates a manifest and derives a stable ID from its content.
func NewManifest(m Manifest) (Manifest, error) {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	m.ID = ""
	// Creation time is useful evidence but must not make equivalent manifests
	// receive different identities.
	hashInput := m
	hashInput.CreatedAt = time.Time{}
	data, err := json.Marshal(hashInput)
	if err != nil {
		return Manifest{}, err
	}
	digest := sha256.Sum256(data)
	m.ID = hex.EncodeToString(digest[:16])
	return m, nil
}
