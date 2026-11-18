// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

// Package openai implements the non-streaming OpenAI Chat Completions protocol.
package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/syzkaller/pkg/aflow/backend"
)

const (
	DefaultRequestTimeout   = 10 * time.Minute
	DefaultMaxOutputTokens  = 16384
	DefaultOutputTokenField = "max_tokens"
	adapterVersion          = "chat-completions-v1"
	maxResponseBytes        = 32 << 20
)

// Config describes a single explicit model on an OpenAI-compatible endpoint.
// Zero timeout/output limit values select the defaults; negative values are invalid.
type Config struct {
	BaseURL          string
	Model            string
	APIKey           string
	RequestTimeout   time.Duration
	MaxOutputTokens  int
	OutputTokenField string
	// HTTPClient may be shared by the caller. Its transport is never closed here.
	HTTPClient *http.Client
}

type Provider struct {
	cfg       Config
	client    *http.Client
	transport *http.Transport
	identity  string
}

var (
	_ backend.Provider              = (*Provider)(nil)
	_ backend.Client                = (*Provider)(nil)
	_ backend.CacheIdentityProvider = (*Provider)(nil)
)

// NewProvider validates configuration without contacting the endpoint.
func NewProvider(cfg Config) (*Provider, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("openai requires an HTTP(S) API base URL without credentials, query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = strings.TrimRight(u.RawPath, "/")
	u.Host = strings.ToLower(u.Host)
	cfg.BaseURL = strings.TrimRight(u.String(), "/")
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.Model == "" {
		return nil, fmt.Errorf("openai requires an explicit model")
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	if cfg.MaxOutputTokens == 0 {
		cfg.MaxOutputTokens = DefaultMaxOutputTokens
	}
	if cfg.OutputTokenField == "" {
		cfg.OutputTokenField = DefaultOutputTokenField
	}
	if cfg.RequestTimeout < 0 || cfg.MaxOutputTokens < 0 {
		return nil, fmt.Errorf("openai timeout and output token limit must be positive")
	}
	if cfg.OutputTokenField != "max_tokens" && cfg.OutputTokenField != "max_completion_tokens" {
		return nil, fmt.Errorf("openai output token field must be max_tokens or max_completion_tokens")
	}
	p := &Provider{cfg: cfg}
	if cfg.HTTPClient != nil {
		clientCopy := *cfg.HTTPClient
		p.client = &clientCopy
	} else {
		p.transport = http.DefaultTransport.(*http.Transport).Clone()
		p.client = &http.Client{Transport: p.transport}
	}
	// A redirect is a configuration error, not a second model request.
	p.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	identity, _ := json.Marshal([]any{adapterVersion, cfg.BaseURL, cfg.Model,
		cfg.MaxOutputTokens, cfg.OutputTokenField})
	p.identity = fmt.Sprintf("openai:%x", sha256.Sum256(identity))
	return p, nil
}

func (p *Provider) Client(ctx context.Context) (backend.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Provider) Models(context.Context) ([]string, error) {
	return []string{p.cfg.Model}, nil
}

func (p *Provider) ResolveModels(category backend.ModelCategory) []string {
	switch category {
	case backend.CoreModel, backend.DeepReasoningModel, backend.LightweightModel:
		return []string{p.cfg.Model}
	default:
		return nil
	}
}

func (p *Provider) CacheIdentity() string { return p.identity }

func (p *Provider) Close() error {
	if p.transport != nil {
		p.transport.CloseIdleConnections()
	}
	return nil
}

func (p *Provider) GenerateContent(ctx context.Context, model string, cfg *backend.GenerateConfig,
	history []*backend.Message) (*backend.GenerateResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if model != p.cfg.Model {
		return nil, fmt.Errorf("openai model %q is not the configured model", model)
	}
	messages, err := toMessages(cfg, history)
	if err != nil {
		return nil, err
	}
	tools, err := toTools(cfg)
	if err != nil {
		return nil, err
	}
	request := map[string]any{
		"model": model, "messages": messages, "stream": false,
		p.cfg.OutputTokenField: p.cfg.MaxOutputTokens,
	}
	if len(tools) != 0 {
		request["tools"] = tools
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("openai request: %w", err)
	}
	timedCtx, cancel := context.WithTimeout(ctx, p.cfg.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(timedCtx, http.MethodPost,
		p.cfg.BaseURL+"/chat/completions", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("openai request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, p.transportError(ctx, timedCtx, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, p.transportError(ctx, timedCtx, err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("openai response exceeds %d bytes", maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp.StatusCode, resp.Header, body, time.Now(), p.cfg.APIKey)
	}
	return fromResponse(body)
}

func (p *Provider) transportError(ctx, timedCtx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Avoid including URLs, headers or reflected credentials in transport errors.
	if timedCtx.Err() != nil || isTemporary(err) {
		return &backend.RetryError{Delay: time.Second, IsExponential: true,
			Err: fmt.Errorf("openai request timed out or encountered a temporary network failure")}
	}
	return fmt.Errorf("openai transport failed (%T)", err)
}
