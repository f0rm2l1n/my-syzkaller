// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package experiment

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// Event is the stable, text-compatible representation emitted by the demo
// driver. Pointer values are intentionally not used as object identity.
type Event struct {
	Seq        int    `json:"seq"`
	Object     string `json:"object"`
	Generation uint64 `json:"generation"`
	CPU        int    `json:"cpu"`
	Task       int    `json:"task"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	Result     int    `json:"result"`
}

// ParseEvents parses lines of the form:
// LLMEXP seq=1 object=ctx generation=2 cpu=0 task=42 kind=submit state=queued result=0
// Unknown lines are ignored so ordinary kernel output remains useful.
func ParseEvents(output string) ([]Event, []string) {
	var events []Event
	var warnings []string
	s := bufio.NewScanner(strings.NewReader(output))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if !strings.HasPrefix(line, "LLMEXP ") {
			continue
		}
		fields := map[string]string{}
		for _, field := range strings.Fields(strings.TrimPrefix(line, "LLMEXP ")) {
			key, value, ok := strings.Cut(field, "=")
			if ok {
				fields[key] = value
			}
		}
		event := Event{Object: fields["object"], Kind: fields["kind"], State: fields["state"]}
		var err error
		if event.Object == "" || event.Kind == "" {
			warnings = append(warnings, "event is missing object or kind")
			continue
		}
		if event.Seq, err = parseInt(fields, "seq"); err != nil {
			warnings = append(warnings, err.Error())
			continue
		}
		if event.Generation, err = parseUint(fields, "generation"); err != nil {
			warnings = append(warnings, err.Error())
			continue
		}
		event.CPU, _ = parseInt(fields, "cpu")
		event.Task, _ = parseInt(fields, "task")
		event.Result, _ = parseInt(fields, "result")
		events = append(events, event)
	}
	if err := s.Err(); err != nil {
		warnings = append(warnings, fmt.Sprintf("scan output: %v", err))
	}
	return events, warnings
}

func parseInt(fields map[string]string, name string) (int, error) {
	v, ok := fields[name]
	if !ok {
		return 0, fmt.Errorf("event is missing %s", name)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", name, v)
	}
	return n, nil
}

func parseUint(fields map[string]string, name string) (uint64, error) {
	v, ok := fields[name]
	if !ok {
		return 0, fmt.Errorf("event is missing %s", name)
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q", name, v)
	}
	return n, nil
}
