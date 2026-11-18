// Copyright 2026 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/syzkaller/pkg/aflow/backend"
	"github.com/stretchr/testify/require"
)

func testConfig() Config {
	return Config{BaseURL: "http://localhost/v1", Model: "test-model"}
}

func TestConfiguration(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"empty-url":    func(c *Config) { c.BaseURL = "" },
		"relative-url": func(c *Config) { c.BaseURL = "/v1" },
		"scheme":       func(c *Config) { c.BaseURL = "file:///v1" },
		"credentials":  func(c *Config) { c.BaseURL = "http://user:secret@localhost/v1" },
		"query":        func(c *Config) { c.BaseURL += "?key=secret" },
		"fragment":     func(c *Config) { c.BaseURL += "#x" },
		"model":        func(c *Config) { c.Model = " " },
		"timeout":      func(c *Config) { c.RequestTimeout = -time.Second },
		"tokens":       func(c *Config) { c.MaxOutputTokens = -1 },
		"field":        func(c *Config) { c.OutputTokenField = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			change(&cfg)
			_, err := NewProvider(cfg)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret")
		})
	}
	p, err := NewProvider(testConfig())
	require.NoError(t, err)
	t.Cleanup(func() { p.Close() })
	require.Equal(t, DefaultRequestTimeout, p.cfg.RequestTimeout)
	require.Equal(t, DefaultMaxOutputTokens, p.cfg.MaxOutputTokens)
	models, err := p.Models(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"test-model"}, models)
	for _, category := range []backend.ModelCategory{backend.CoreModel, backend.LightweightModel, backend.DeepReasoningModel} {
		require.Equal(t, models, p.ResolveModels(category))
	}
	require.Nil(t, p.ResolveModels("unknown"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.Client(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCacheIdentity(t *testing.T) {
	identity := func(cfg Config) string {
		p, err := NewProvider(cfg)
		require.NoError(t, err)
		t.Cleanup(func() { p.Close() })
		return p.CacheIdentity()
	}
	base := identity(testConfig())
	for name, change := range map[string]func(*Config){
		"endpoint": func(c *Config) { c.BaseURL += "/other" },
		"model":    func(c *Config) { c.Model = "other-model" },
		"limit":    func(c *Config) { c.MaxOutputTokens = 32 },
		"field":    func(c *Config) { c.OutputTokenField = "max_completion_tokens" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			change(&cfg)
			require.NotEqual(t, base, identity(cfg))
		})
	}
	cfg := testConfig()
	cfg.BaseURL += "/"
	cfg.APIKey = "private-key"
	require.Equal(t, base, identity(cfg))
	require.NotContains(t, identity(cfg), cfg.APIKey)
}

func TestHTTPRoundTrip(t *testing.T) {
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		for _, key := range []string{"", "test-key"} {
			t.Run(field+"/"+key, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.Equal(t, "/prefix/v1/chat/completions", r.URL.Path)
					require.Equal(t, http.MethodPost, r.Method)
					require.Equal(t, "application/json", r.Header.Get("Content-Type"))
					wantAuth := ""
					if key != "" {
						wantAuth = "Bearer " + key
					}
					require.Equal(t, wantAuth, r.Header.Get("Authorization"))
					var req map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
					require.Equal(t, "test-model", req["model"])
					require.Equal(t, false, req["stream"])
					require.Equal(t, float64(123), req[field])
					require.Len(t, req, 4)
					fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],`+
						`"usage":{"prompt_tokens":7,"completion_tokens":9,"completion_tokens_details":{"reasoning_tokens":4}}}`)
				}))
				defer server.Close()
				cfg := testConfig()
				cfg.BaseURL, cfg.APIKey = server.URL+"/prefix/v1/", key
				cfg.OutputTokenField, cfg.MaxOutputTokens = field, 123
				p, err := NewProvider(cfg)
				require.NoError(t, err)
				defer p.Close()
				resp, err := p.GenerateContent(context.Background(), cfg.Model, nil,
					[]*backend.Message{{Role: backend.RoleUser, Parts: []backend.Part{{Text: "hello"}}}})
				require.NoError(t, err)
				require.Equal(t, []backend.Part{{Text: "done"}}, resp.Parts)
				require.Equal(t, &backend.UsageMetadata{InputTokens: 7, OutputTokens: 9}, resp.UsageMetadata)
			})
		}
	}
}

func TestHTTPErrorNoRetry(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "4")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":"rate_limit_exceeded","message":"test-key"}}`)
	}))
	defer server.Close()
	cfg := testConfig()
	cfg.BaseURL, cfg.APIKey = server.URL, "test-key"
	p, err := NewProvider(cfg)
	require.NoError(t, err)
	defer p.Close()
	_, err = p.GenerateContent(context.Background(), cfg.Model, nil,
		[]*backend.Message{{Role: backend.RoleUser, Parts: []backend.Part{{Text: "hello"}}}})
	var retry *backend.RetryError
	require.ErrorAs(t, err, &retry)
	require.Equal(t, 4*time.Second, retry.Delay)
	require.NotContains(t, err.Error(), cfg.APIKey)
	require.EqualValues(t, 1, requests.Load())
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRequestCancellation(t *testing.T) {
	for _, parentCancel := range []bool{false, true} {
		t.Run(fmt.Sprint(parentCancel), func(t *testing.T) {
			started := make(chan struct{})
			cfg := testConfig()
			cfg.RequestTimeout = 20 * time.Millisecond
			cfg.HTTPClient = &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
				close(started)
				<-req.Context().Done()
				return nil, req.Context().Err()
			})}
			p, err := NewProvider(cfg)
			require.NoError(t, err)
			defer p.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if parentCancel {
				p.cfg.RequestTimeout = time.Minute
				go func() { <-started; cancel() }()
			}
			_, err = p.GenerateContent(ctx, cfg.Model, nil,
				[]*backend.Message{{Role: backend.RoleUser, Parts: []backend.Part{{Text: "hello"}}}})
			if parentCancel {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				var retry *backend.RetryError
				require.ErrorAs(t, err, &retry)
			}
		})
	}
}

func TestProviderConcurrentRequests(t *testing.T) {
	cfg := testConfig()
	cfg.HTTPClient = &http.Client{Transport: roundTripper(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Messages []message `json:"messages"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		response, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{
				"role": "assistant", "content": body.Messages[0].Content}, "finish_reason": "stop"}},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1},
		})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(response)))}, nil
	})}
	p, err := NewProvider(cfg)
	require.NoError(t, err)
	defer p.Close()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			text := fmt.Sprintf("session %d", i)
			res, err := p.GenerateContent(context.Background(), cfg.Model, nil,
				[]*backend.Message{{Role: backend.RoleUser, Parts: []backend.Part{{Text: text}}}})
			if !assertResponse(t, res, err, text) {
				return
			}
		})
	}
	wg.Wait()
}

func assertResponse(t *testing.T, res *backend.GenerateResponse, err error, text string) bool {
	t.Helper()
	if err != nil || res == nil || len(res.Parts) != 1 || res.Parts[0].Text != text {
		t.Errorf("response mismatch: %+v, %v, want %q", res, err, text)
		return false
	}
	return true
}

func TestHTTPRedirectIsNotFollowed(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	cfg := testConfig()
	cfg.BaseURL = source.URL
	p, err := NewProvider(cfg)
	require.NoError(t, err)
	defer p.Close()
	_, err = p.GenerateContent(context.Background(), cfg.Model, nil,
		[]*backend.Message{{Role: backend.RoleUser, Parts: []backend.Part{{Text: "hello"}}}})
	require.Error(t, err)
	require.Zero(t, targetRequests.Load())
	var retry *backend.RetryError
	require.False(t, errors.As(err, &retry))
}
