// Package model talks to a locally hosted language model.
//
// The extraction harness treats a model as a compiler: it is invoked when a
// source is first defined and when an artifact has demonstrably stopped
// working, and never on the steady-state path. That changes what matters in a
// client. Throughput is irrelevant — this makes single-digit numbers of calls
// per day. Determinism matters, so the temperature is zero. Patience matters,
// because a 7B model on a Raspberry Pi answering a 20,000-token prompt takes
// minutes, and a client that gives up at thirty seconds turns a slow success
// into a failure that looks like a broken source.
//
// The wire format is OpenAI's chat-completions, which Ollama, llama.cpp,
// vLLM and LM Studio all serve. That is the only reason it was chosen: it
// means the operator can change what is answering without changing this.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Defaults for a self-hosted setup. The endpoint is Ollama's OpenAI-compatible
// path on the loopback address, which is what a homelab deployment has.
const (
	DefaultEndpoint = "http://localhost:11434/v1"
	DefaultModel    = "qwen2.5-coder:7b"
	DefaultTimeout  = 10 * time.Minute
)

// maxErrorBody caps how much of a failure response is quoted back in an error.
const maxErrorBody = 2048

// Client is a chat-completions client.
type Client struct {
	endpoint string
	model    string
	key      string
	http     *http.Client
}

// New builds a client from the environment.
//
// Settings come from the environment rather than from flags for the same
// reason the rest of this tool's do: one set of variables configures every
// subcommand identically, whether it runs from a shell, a container or a
// scheduler.
//
//	EXTRACT_MODEL_ENDPOINT  OpenAI-compatible base URL
//	EXTRACT_MODEL           model name
//	EXTRACT_MODEL_KEY       bearer token, if the server wants one
//	EXTRACT_MODEL_TIMEOUT   per-request timeout, as a Go duration
func New() (*Client, error) {
	endpoint := strings.TrimRight(envOr("EXTRACT_MODEL_ENDPOINT", DefaultEndpoint), "/")
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return nil, fmt.Errorf("EXTRACT_MODEL_ENDPOINT %q is not an http or https URL", endpoint)
	}

	timeout := DefaultTimeout
	if raw := os.Getenv("EXTRACT_MODEL_TIMEOUT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("EXTRACT_MODEL_TIMEOUT %q: %w", raw, err)
		}
		timeout = parsed
	}

	return &Client{
		endpoint: endpoint,
		model:    envOr("EXTRACT_MODEL", DefaultModel),
		key:      os.Getenv("EXTRACT_MODEL_KEY"),
		http:     &http.Client{Timeout: timeout},
	}, nil
}

// Name returns the model this client asks for, for provenance.
func (c *Client) Name() string { return c.model }

// Endpoint returns the configured base URL, for the CLI to show an operator
// what a generation is about to talk to.
func (c *Client) Endpoint() string { return c.endpoint }

// ErrEmpty means the server answered with no content, which some local servers
// do when a model has been unloaded or a context limit was hit.
var ErrEmpty = errors.New("model returned no content")

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	Temperature float64   `json:"temperature"`
	Stream      bool      `json:"stream"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends a system and user prompt and returns the reply.
//
// Temperature is pinned to zero. The output is compiled into a stored artifact
// and diffed against its predecessor when it changes, and sampling would make
// every regeneration produce a gratuitously different script for an operator to
// review.
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	body, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []message{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		Temperature: 0,
		Stream:      false,
	})
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}

	url := c.endpoint + "/chat/completions"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if c.key != "" {
		request.Header.Set("Authorization", "Bearer "+c.key)
	}

	response, err := c.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("call %s: %w", url, err)
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("read response from %s: %w", url, err)
	}

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s: %s",
			url, response.Status, truncate(string(payload), maxErrorBody))
	}

	var decoded chatResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("decode response from %s: %w", url, err)
	}
	if decoded.Error != nil {
		return "", fmt.Errorf("%s reported: %s", url, decoded.Error.Message)
	}
	if len(decoded.Choices) == 0 {
		return "", fmt.Errorf("%w: %s returned no choices", ErrEmpty, url)
	}

	content := decoded.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		// A local server that has run out of context answers with an empty
		// string and finish_reason "length" rather than with an error, so the
		// reason is worth reporting: it is the difference between "the model
		// is wrong" and "the prompt did not fit".
		return "", fmt.Errorf("%w (finish reason %q)", ErrEmpty, decoded.Choices[0].FinishReason)
	}
	return content, nil
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "… (" + strconv.Itoa(len(s)) + " bytes)"
}
