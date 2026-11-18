// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/syzkaller/pkg/aflow/backend"
)

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type tool struct {
	Type     string   `json:"type"`
	Function function `json:"function"`
}

type function struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func toTools(cfg *backend.GenerateConfig) ([]tool, error) {
	var ret []tool
	if cfg == nil {
		return ret, nil
	}
	for _, group := range cfg.Tools {
		for _, decl := range group.FunctionDeclarations {
			params, err := json.Marshal(decl.ParametersJSONSchema)
			if err != nil {
				return nil, fmt.Errorf("openai tool schema: %w", err)
			}
			description := decl.Description
			if decl.ResponseJSONSchema != nil {
				result, err := json.Marshal(decl.ResponseJSONSchema)
				if err != nil {
					return nil, fmt.Errorf("openai result schema: %w", err)
				}
				description += "\nTool result JSON schema: " + string(result)
			}
			ret = append(ret, tool{Type: "function", Function: function{
				Name: decl.Name, Description: description, Parameters: params,
			}})
		}
	}
	return ret, nil
}

func textParts(msg *backend.Message) string {
	var text []string
	for _, part := range msg.Parts {
		if part.Text != "" && !part.Thought {
			text = append(text, part.Text)
		}
	}
	return strings.Join(text, "\n")
}

// toMessages keeps complete call/result groups in native tool format. aflow can
// replace results during wrap-up, so incomplete historical groups are represented
// as text, explicitly retaining the distinction between a call and an observation.
func toMessages(cfg *backend.GenerateConfig, history []*backend.Message) ([]message, error) {
	var ret []message
	if cfg != nil && cfg.SystemInstruction != nil {
		ret = append(ret, message{Role: "system", Content: textParts(cfg.SystemInstruction)})
	}
	for i := 0; i < len(history); i++ {
		msg := history[i]
		if msg == nil {
			return nil, fmt.Errorf("openai history contains a nil message")
		}
		switch msg.Role {
		case backend.RoleModel:
			calls, err := historyCalls(msg)
			if err != nil {
				return nil, err
			}
			var results []message
			if len(calls) != 0 && i+1 < len(history) {
				results = pairedResults(calls, history[i+1])
			}
			if results != nil {
				ret = append(ret, message{Role: "assistant", Content: textParts(msg), ToolCalls: calls})
				ret = append(ret, results...)
				i++
				if text := textParts(history[i]); text != "" {
					ret = append(ret, message{Role: "user", Content: text})
				}
			} else {
				text := textParts(msg)
				for _, call := range calls {
					text += fmt.Sprintf("\nHistorical tool call (result not paired in retained history): "+
						"id=%s name=%s arguments=%s", call.ID, call.Function.Name, call.Function.Arguments)
				}
				if text != "" {
					ret = append(ret, message{Role: "assistant", Content: text})
				}
			}
		case backend.RoleUser:
			text := textParts(msg)
			for _, part := range msg.Parts {
				if result := part.FunctionResponse; result != nil {
					data, err := json.Marshal(result.Response)
					if err != nil {
						return nil, fmt.Errorf("openai historical tool result: %w", err)
					}
					text += fmt.Sprintf("\nHistorical tool result (call not paired in retained history): "+
						"id=%s name=%s result=%s", result.ID, result.Name, data)
				}
			}
			if text != "" {
				ret = append(ret, message{Role: "user", Content: text})
			}
		default:
			return nil, fmt.Errorf("openai unsupported history role %q", msg.Role)
		}
	}
	if len(ret) == 0 {
		return nil, fmt.Errorf("openai requires a non-empty conversation")
	}
	return ret, nil
}

func historyCalls(msg *backend.Message) ([]toolCall, error) {
	var calls []toolCall
	for _, part := range msg.Parts {
		if call := part.FunctionCall; call != nil {
			args := call.Args
			if args == nil {
				args = map[string]any{}
			}
			data, err := json.Marshal(args)
			if err != nil {
				return nil, fmt.Errorf("openai tool arguments: %w", err)
			}
			calls = append(calls, toolCall{ID: call.ID, Type: "function",
				Function: functionCall{Name: call.Name, Arguments: string(data)}})
		}
	}
	return calls, nil
}

func pairedResults(calls []toolCall, msg *backend.Message) []message {
	if msg == nil || msg.Role != backend.RoleUser {
		return nil
	}
	wanted := make(map[string]string)
	for _, call := range calls {
		if call.ID == "" || call.Function.Name == "" || wanted[call.ID] != "" {
			return nil
		}
		wanted[call.ID] = call.Function.Name
	}
	var ret []message
	for _, part := range msg.Parts {
		result := part.FunctionResponse
		if result == nil {
			continue
		}
		if result.Name == "" || wanted[result.ID] != result.Name {
			return nil
		}
		data, err := json.Marshal(result.Response)
		if err != nil {
			return nil // The textual fallback reports serialization errors.
		}
		ret = append(ret, message{Role: "tool", Content: string(data), ToolCallID: result.ID})
		delete(wanted, result.ID)
	}
	if len(wanted) != 0 {
		return nil
	}
	return ret
}

func fromResponse(body []byte) (*backend.GenerateResponse, error) {
	var response struct {
		Choices []struct {
			Message struct {
				Role      string     `json:"role"`
				Content   *string    `json:"content"`
				Refusal   *string    `json:"refusal"`
				ToolCalls []toolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("openai returned invalid response JSON (%T)", err)
	}
	if len(response.Choices) != 1 {
		return nil, fmt.Errorf("openai expected one response choice, got %d", len(response.Choices))
	}
	choice := response.Choices[0]
	if choice.FinishReason == "content_filter" ||
		(choice.Message.Refusal != nil && *choice.Message.Refusal != "") {
		return nil, &backend.BlockedError{Err: fmt.Errorf("openai response was refused")}
	}
	switch choice.FinishReason {
	case "length":
		return nil, &backend.OutputTokenOverflowError{Err: fmt.Errorf("openai output token limit reached")}
	case "stop", "tool_calls":
	default:
		return nil, fmt.Errorf("openai unsupported finish reason %q", choice.FinishReason)
	}
	if choice.Message.Role != "assistant" {
		return nil, fmt.Errorf("openai expected an assistant response")
	}
	usage := response.Usage
	if usage == nil || usage.PromptTokens == nil || usage.CompletionTokens == nil ||
		*usage.PromptTokens < 0 || *usage.CompletionTokens < 0 {
		return nil, fmt.Errorf("openai response is missing valid prompt_tokens/completion_tokens usage")
	}
	ret := &backend.GenerateResponse{UsageMetadata: &backend.UsageMetadata{
		InputTokens: *usage.PromptTokens, OutputTokens: *usage.CompletionTokens,
	}}
	if choice.Message.Content != nil && strings.TrimSpace(*choice.Message.Content) != "" {
		ret.Parts = append(ret.Parts, backend.Part{Text: *choice.Message.Content})
	}
	seen := make(map[string]bool)
	for _, call := range choice.Message.ToolCalls {
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Function.Name) == "" ||
			call.Type != "function" || seen[call.ID] {
			return nil, fmt.Errorf("openai returned an invalid or duplicate tool call")
		}
		seen[call.ID] = true
		var args map[string]any
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args == nil {
			return nil, fmt.Errorf("openai tool arguments must be a valid JSON object")
		}
		ret.Parts = append(ret.Parts, backend.Part{FunctionCall: &backend.FunctionCall{
			ID: call.ID, Name: call.Function.Name, Args: args,
		}})
	}
	if len(ret.Parts) == 0 || (choice.FinishReason == "tool_calls" && len(seen) == 0) {
		return nil, fmt.Errorf("openai returned an empty or inconsistent response")
	}
	return ret, nil
}
