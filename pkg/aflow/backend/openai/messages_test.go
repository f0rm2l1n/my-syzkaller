// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package openai

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/syzkaller/pkg/aflow/backend"
	"github.com/stretchr/testify/require"
)

func TestMessagesToolGroups(t *testing.T) {
	call := func(id string) backend.Part {
		return backend.Part{FunctionCall: &backend.FunctionCall{ID: id, Name: "read", Args: map[string]any{"path": "a\"b"}}}
	}
	result := func(id string) backend.Part {
		return backend.Part{FunctionResponse: &backend.FunctionResponse{ID: id, Name: "read", Response: map[string]any{"value": id}}}
	}
	history := []*backend.Message{
		{Role: backend.RoleUser, Parts: []backend.Part{{Text: "anchor and compressed summary"}}},
		{Role: backend.RoleModel, Parts: []backend.Part{{Text: "hidden", Thought: true}, {Text: "reading"}, call("a"), call("b")}},
		{Role: backend.RoleUser, Parts: []backend.Part{{Text: "advisor"}, {Text: "compression reminder"}, result("b"), result("a")}},
	}
	before, err := json.Marshal(history)
	require.NoError(t, err)
	got, err := toMessages(&backend.GenerateConfig{SystemInstruction: &backend.Message{
		Parts: []backend.Part{{Text: "system"}},
	}}, history)
	require.NoError(t, err)
	require.Len(t, got, 6)
	require.Equal(t, "system", got[0].Role)
	require.Equal(t, "reading", got[2].Content)
	require.Len(t, got[2].ToolCalls, 2)
	require.Equal(t, "tool", got[3].Role)
	require.Equal(t, "b", got[3].ToolCallID)
	require.Equal(t, "a", got[4].ToolCallID)
	require.Equal(t, "user", got[5].Role)
	require.Equal(t, "advisor\ncompression reminder", got[5].Content)
	after, err := json.Marshal(history)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "hidden")
	for name, parts := range map[string][]backend.Part{
		"replaced":  {{Text: "answer now"}},
		"partial":   {result("a"), {Text: "answer now"}},
		"duplicate": {result("a"), result("a")},
		"orphan":    {result("c")},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := toMessages(nil, []*backend.Message{history[1], {Role: backend.RoleUser, Parts: parts}})
			require.NoError(t, err)
			for _, msg := range got {
				require.Empty(t, msg.ToolCalls)
				require.NotEqual(t, "tool", msg.Role)
			}
			require.Contains(t, got[0].Content, "result not paired in retained history")
		})
	}
	// Upstream rejects more than ten calls with a textual reminder and no results.
	var many []backend.Part
	for i := range 11 {
		many = append(many, call(fmt.Sprint(i)))
	}
	got, err = toMessages(nil, []*backend.Message{{Role: backend.RoleModel, Parts: many},
		{Role: backend.RoleUser, Parts: []backend.Part{{Text: "too many calls"}}}})
	require.NoError(t, err)
	require.Empty(t, got[0].ToolCalls)
	require.Contains(t, got[1].Content, "too many calls")
}

func TestToolSchemas(t *testing.T) {
	var schema jsonschema.Schema
	require.NoError(t, json.Unmarshal([]byte(`{"type":"object","properties":{"files":{"type":"array",`+
		`"items":{"$ref":"#/$defs/file"}},"optional":{"type":"string"}},"required":["files"],`+
		`"$defs":{"file":{"type":"string"}}}`), &schema))
	before, err := json.Marshal(schema)
	require.NoError(t, err)
	tools, err := toTools(&backend.GenerateConfig{Tools: []*backend.Tool{{
		FunctionDeclarations: []*backend.FunctionDeclaration{{Name: "read", Description: "read files",
			ParametersJSONSchema: &schema, ResponseJSONSchema: &schema}},
	}}})
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(tools[0].Function.Parameters))
	require.Contains(t, tools[0].Function.Description, "Tool result JSON schema:")
	after, err := json.Marshal(schema)
	require.NoError(t, err)
	require.Equal(t, string(before), string(after))
}

func TestResponseValidation(t *testing.T) {
	makeResponse := func(msg map[string]any, reason string, usage any) []byte {
		data, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"message": msg, "finish_reason": reason}}, "usage": usage})
		require.NoError(t, err)
		return data
	}
	validUsage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0}
	for name, test := range map[string]struct {
		message map[string]any
		reason  string
		usage   any
	}{
		"missing-usage":  {map[string]any{"role": "assistant", "content": "ok"}, "stop", nil},
		"missing-count":  {map[string]any{"role": "assistant", "content": "ok"}, "stop", map[string]int{"prompt_tokens": 0}},
		"negative-count": {map[string]any{"role": "assistant", "content": "ok"}, "stop", map[string]int{"prompt_tokens": -1, "completion_tokens": 1}},
		"empty":          {map[string]any{"role": "assistant", "content": " "}, "stop", validUsage},
		"role":           {map[string]any{"role": "user", "content": "ok"}, "stop", validUsage},
		"finish":         {map[string]any{"role": "assistant", "content": "ok"}, "unexpected", validUsage},
		"missing-calls":  {map[string]any{"role": "assistant", "content": "ok"}, "tool_calls", validUsage},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := fromResponse(makeResponse(test.message, test.reason, test.usage))
			require.Error(t, err)
		})
	}
	call := toolCall{ID: "id", Type: "function", Function: functionCall{Name: "read", Arguments: `{"n":1}`}}
	for _, args := range []string{"{", "null", "[]", `"text"`, `{} trailing`} {
		bad := call
		bad.Function.Arguments = args
		_, err := fromResponse(makeResponse(map[string]any{"role": "assistant", "tool_calls": []toolCall{bad}}, "tool_calls", validUsage))
		require.Error(t, err)
	}
	for _, change := range []func(*toolCall){
		func(c *toolCall) { c.ID = "" }, func(c *toolCall) { c.Function.Name = "" }, func(c *toolCall) { c.Type = "custom" },
	} {
		bad := call
		change(&bad)
		_, err := fromResponse(makeResponse(map[string]any{"role": "assistant", "tool_calls": []toolCall{bad}}, "tool_calls", validUsage))
		require.Error(t, err)
	}
	_, err := fromResponse(makeResponse(map[string]any{"role": "assistant", "tool_calls": []toolCall{call, call}}, "tool_calls", validUsage))
	require.Error(t, err)
	res, err := fromResponse(makeResponse(map[string]any{"role": "assistant", "content": "working", "tool_calls": []toolCall{call}}, "tool_calls", validUsage))
	require.NoError(t, err)
	require.Len(t, res.Parts, 2)
	require.Equal(t, "id", res.Parts[1].FunctionCall.ID)
	require.Equal(t, float64(1), res.Parts[1].FunctionCall.Args["n"])
	for reason, want := range map[string]any{"length": &backend.OutputTokenOverflowError{}, "content_filter": &backend.BlockedError{}} {
		_, err := fromResponse(makeResponse(map[string]any{}, reason, nil))
		require.IsType(t, want, err)
	}
	_, err = fromResponse(makeResponse(map[string]any{"refusal": "no"}, "stop", nil))
	require.IsType(t, &backend.BlockedError{}, err)
	for _, body := range []string{"{", `{"choices":[]}`, `{"choices":[{},{}]}`} {
		_, err := fromResponse([]byte(body))
		require.Error(t, err)
	}
}
