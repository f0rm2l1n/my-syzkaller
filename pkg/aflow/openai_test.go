// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package aflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/syzkaller/pkg/aflow/backend"
	"github.com/google/syzkaller/pkg/aflow/backend/openai"
	"github.com/google/syzkaller/pkg/aflow/trajectory"
	"github.com/stretchr/testify/require"
)

// These fixtures exercise the real agent loop and HTTP adapter, without VM tools
// or stubbed model calls. The wire history must satisfy Chat Completions ordering.
type openAIRequest struct {
	Messages []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools []json.RawMessage `json:"tools"`
}

func checkOpenAIHistory(t *testing.T, req *openAIRequest) {
	t.Helper()
	pending := make(map[string]bool)
	for _, msg := range req.Messages {
		if len(pending) > 0 {
			require.Equal(t, "tool", msg.Role, "all results must precede reminder messages")
		}
		if msg.Role == "tool" {
			require.True(t, pending[msg.ToolCallID], "orphan tool result")
			delete(pending, msg.ToolCallID)
		}
		for _, call := range msg.ToolCalls {
			require.Equal(t, "assistant", msg.Role)
			require.False(t, pending[call.ID])
			pending[call.ID] = true
		}
	}
	require.Empty(t, pending)
}

func openAICall(id, name, args string) map[string]any {
	return map[string]any{"id": id, "type": "function", "function": map[string]string{
		"name": name, "arguments": args,
	}}
}

func writeOpenAIResponse(t *testing.T, w http.ResponseWriter, text string, calls []map[string]any, input int) {
	t.Helper()
	reason := "stop"
	if len(calls) > 0 {
		reason = "tool_calls"
	}
	err := json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{
			"role": "assistant", "content": text, "tool_calls": calls}, "finish_reason": reason}},
		"usage": map[string]int{"prompt_tokens": input, "completion_tokens": 10},
	})
	require.NoError(t, err)
}

func openAIProvider(t *testing.T, url string) *openai.Provider {
	t.Helper()
	p, err := openai.NewProvider(openai.Config{BaseURL: url, Model: "test-model"})
	require.NoError(t, err)
	t.Cleanup(func() { p.Close() })
	return p
}

func registerOpenAIFlow(t *testing.T, agent *LLMAgent, structured bool) *Flow {
	t.Helper()
	flow := &Flow{Root: agent}
	var err error
	if structured {
		err = register[struct{}, struct {
			Answer string
			Reply  string
		}]("openai-test", "local test",
			make(map[string]*Flow), []*Flow{flow})
	} else if agent.Judge != nil {
		err = register[struct{}, struct {
			Reply string
			JudgeExecutionResults
		}]("openai-test", "local test", make(map[string]*Flow), []*Flow{flow})
	} else {
		err = register[struct{}, struct{ Reply string }]("openai-test", "local test",
			make(map[string]*Flow), []*Flow{flow})
	}
	require.NoError(t, err)
	return flow
}

func TestOpenAIFlowAndCache(t *testing.T) {
	var requests, executions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openAIRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		checkOpenAIHistory(t, &req)
		switch requests.Add(1) {
		case 1:
			writeOpenAIResponse(t, w, "", []map[string]any{
				openAICall("a", "lookup", `{"Number":1}`), openAICall("b", "lookup", `{"Number":2}`),
			}, 20)
		case 2:
			var results []string
			for _, msg := range req.Messages {
				if msg.Role == "tool" {
					results = append(results, msg.Content)
				}
			}
			require.Equal(t, []string{`{"Value":1}`, `{"Value":2}`}, results)
			writeOpenAIResponse(t, w, "", []map[string]any{openAICall("c", "set-results", `{"Answer":"3"}`)}, 40)
		case 3:
			writeOpenAIResponse(t, w, "done", nil, 50)
		default:
			t.Errorf("unexpected HTTP request")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	tool := NewFuncTool("lookup", func(ctx *Context, state struct{}, args struct {
		Number int `jsonschema:"Number to look up."`
	}) (struct {
		Value int `jsonschema:"Returned number."`
	}, error) {
		executions.Add(1)
		return struct {
			Value int `jsonschema:"Returned number."`
		}{args.Number}, nil
	}, "Return a deterministic number.")
	flow := registerOpenAIFlow(t, &LLMAgent{Name: "local", Model: CoreModel, Reply: "Reply",
		Instruction: "Use tools to add two numbers.", Prompt: "Add 1 and 2.", Tools: Tools(tool),
		Outputs: LLMOutputs[struct {
			Answer string `jsonschema:"The sum."`
		}](),
	}, true)
	p := openAIProvider(t, server.URL)
	workdir := t.TempDir()
	cache, err := NewCache(workdir+"/cache", 1<<20)
	require.NoError(t, err)
	for range 2 {
		var tokens int
		output, err := flow.Execute(t.Context(), nil, ExecuteOptions{Provider: p, Workdir: workdir, Cache: cache,
			OnEvent: func(span *trajectory.Span) error {
				if span.Type == trajectory.SpanLLM && !span.Finished.IsZero() {
					tokens += span.InputTokens + span.OutputTokens
				}
				return nil
			},
		})
		require.NoError(t, err)
		require.Equal(t, map[string]any{"Answer": "3", "Reply": "done"}, output)
		require.Equal(t, 140, tokens)
	}
	require.EqualValues(t, 3, requests.Load(), "second execution must reuse model responses")
	require.EqualValues(t, 4, executions.Load(), "tool execution still runs on cache replay")
	_, err = flow.Execute(t.Context(), nil, ExecuteOptions{Provider: p, Workdir: workdir, Cache: cache, TokenLimit: 1,
		OnEvent: func(*trajectory.Span) error { return nil },
	})
	require.ErrorContains(t, err, "token limit")
	require.EqualValues(t, 4, executions.Load(), "budget failure must precede tool execution")
}

func TestOpenAIFlowHistoryTransitions(t *testing.T) {
	for _, mode := range []string{"compression", "context-overflow", "iteration-limit", "too-many-calls", "supervision"} {
		t.Run(mode, func(t *testing.T) {
			var requests, executions atomic.Int32
			var compressed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req openAIRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				checkOpenAIHistory(t, &req)
				request := requests.Add(1)
				if mode == "compression" && len(req.Tools) == 0 {
					compressed.Store(true)
					writeOpenAIResponse(t, w, "previous calls completed", nil, 20)
					return
				}
				if mode == "context-overflow" && request == 2 {
					w.WriteHeader(400)
					fmt.Fprint(w, `{"error":{"code":"context_length_exceeded"}}`)
					return
				}
				if mode == "supervision" && request == 3 {
					writeOpenAIResponse(t, w, "", []map[string]any{
						openAICall("judge", "set-results", `{"Stop":false,"Reason":"making progress"}`),
					}, 20)
					return
				}
				if mode == "supervision" && request == 4 {
					writeOpenAIResponse(t, w, "", []map[string]any{
						openAICall("advisor", "set-results", `{"Advice":"finish the task"}`),
					}, 20)
					return
				}
				if request == 1 || (mode == "compression" || mode == "supervision") && request == 2 {
					calls := []map[string]any{openAICall(fmt.Sprint(request), "tick", "{}")}
					if mode == "too-many-calls" {
						calls = nil
						for i := range 11 {
							calls = append(calls, openAICall(fmt.Sprint(i), "tick", "{}"))
						}
					}
					input := 20
					if request == 2 {
						input = 300
					}
					writeOpenAIResponse(t, w, "", calls, input)
					return
				}
				if mode == "context-overflow" {
					require.Contains(t, req.Messages[len(req.Messages)-1].Content, "final answer now")
					for _, msg := range req.Messages {
						require.Empty(t, msg.ToolCalls)
					}
				}
				if mode == "supervision" {
					require.Contains(t, req.Messages[len(req.Messages)-1].Content, "<supervisor_advice>")
				}
				writeOpenAIResponse(t, w, "done", nil, 20)
			}))
			defer server.Close()
			tool := NewFuncTool("tick", func(ctx *Context, state, args struct{}) (struct{}, error) {
				executions.Add(1)
				return struct{}{}, nil
			}, "Perform a deterministic operation.")
			agent := &LLMAgent{Name: "local", Model: CoreModel, Reply: "Reply", Instruction: "Use tick.",
				Prompt: "Run a local task.", Tools: Tools(tool), AllowPartialResults: true}
			if mode == "compression" {
				agent.compressTokens = 100
			}
			if mode == "iteration-limit" {
				agent.MaxIterations = 1
			}
			if mode == "supervision" {
				agent.Judge = &LLMJudge{Name: "judge", Model: DeepReasoningModel, Instruction: "Check progress.",
					MinIterations: 1, EvaluationInterval: 1}
				agent.Advisor = &LLMAdvisor{Name: "advisor", Model: LightweightModel, Instruction: "Give advice.",
					MinIterations: 1, EvaluationInterval: 1}
			}
			flow := registerOpenAIFlow(t, agent, false)
			workdir := t.TempDir()
			cache, err := NewCache(workdir+"/cache", 1<<20)
			require.NoError(t, err)
			out, err := flow.Execute(t.Context(), nil, ExecuteOptions{Provider: openAIProvider(t, server.URL),
				Workdir: workdir, Cache: cache, OnEvent: func(*trajectory.Span) error { return nil }})
			require.NoError(t, err)
			require.Equal(t, "done", out["Reply"])
			if mode == "compression" {
				require.True(t, compressed.Load())
			}
			if mode == "too-many-calls" {
				require.Zero(t, executions.Load())
			}
		})
	}
}

type cacheIdentityTestProvider struct {
	dummyProvider
	identity string
}

func (p *cacheIdentityTestProvider) CacheIdentity() string { return p.identity }

func TestLLMOptionalProviderCacheIdentity(t *testing.T) {
	ctx := NewTestContext(t)
	t.Cleanup(ctx.Close)
	var calls int
	ctx.generateContent = func(string, *backend.GenerateConfig, []*backend.Message) (*backend.GenerateResponse, error) {
		calls++
		return &backend.GenerateResponse{Parts: []backend.Part{{Text: fmt.Sprint(calls)}}}, nil
	}
	agent := &LLMAgent{}
	get := func(provider backend.Provider) string {
		ctx.provider = provider
		res, err := agent.generateContentCached(ctx, &backend.GenerateConfig{}, nil, 0, 0, "same-model")
		require.NoError(t, err)
		return res.Parts[0].Text
	}
	require.Equal(t, "1", get(&dummyProvider{}))
	require.Equal(t, "1", get(&dummyProvider{}))
	first := &cacheIdentityTestProvider{identity: "endpoint-one"}
	second := &cacheIdentityTestProvider{identity: "endpoint-two"}
	require.Equal(t, "2", get(first))
	require.Equal(t, "3", get(second))
	require.Equal(t, "2", get(first))
	require.Equal(t, "1", get(&dummyProvider{}))
	require.Equal(t, 3, calls)
}

// Keep cancellation coverage at the provider boundary: aflow's existing backoff
// sleep behavior is unchanged by this integration.
func TestOpenAICanceledFlow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := openAIProvider(t, "http://localhost/v1").Client(ctx)
	require.ErrorIs(t, err, context.Canceled)
}
