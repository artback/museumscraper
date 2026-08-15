package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ClaudeCode generates through the Claude Code CLI installed on this machine.
//
// It exists because the PRD allows escalating to a stronger model at
// generation time — generation is rare, so its cost barely matters, while its
// quality decides whether a source can be collected at all. A 7B model on a Pi
// writes a working extractor for a plain listing page and gives up on a
// complicated one; when it does, this is the fallback that does not require
// standing up an inference server.
//
// It is not the steady-state path and must not become one. Nothing here is
// reached on a passing run.
type ClaudeCode struct {
	binary  string
	model   string
	timeout time.Duration
}

// Defaults for the CLI backend. The timeout is long because a large reduced
// page is a large prompt, and because this is called a handful of times per
// day at most.
const (
	DefaultClaudeBinary  = "claude"
	DefaultClaudeTimeout = 10 * time.Minute
)

// ErrNoClaudeCode means the CLI is not installed or not on PATH.
var ErrNoClaudeCode = errors.New("the claude CLI was not found")

// NewClaudeCode returns a generator backed by the local Claude Code CLI.
//
//	EXTRACT_CLAUDE_BINARY   path to the CLI, default "claude" on PATH
//	EXTRACT_CLAUDE_MODEL    model to ask for, default the CLI's own
//	EXTRACT_MODEL_TIMEOUT   per-generation timeout
func NewClaudeCode() (*ClaudeCode, error) {
	binary := envOr("EXTRACT_CLAUDE_BINARY", DefaultClaudeBinary)

	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNoClaudeCode, binary)
	}

	timeout := DefaultClaudeTimeout
	if raw := os.Getenv("EXTRACT_MODEL_TIMEOUT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("EXTRACT_MODEL_TIMEOUT %q: %w", raw, err)
		}
		timeout = parsed
	}

	return &ClaudeCode{
		binary:  resolved,
		model:   os.Getenv("EXTRACT_CLAUDE_MODEL"),
		timeout: timeout,
	}, nil
}

// Name identifies the backend for an artifact's provenance.
func (c *ClaudeCode) Name() string {
	if c.model != "" {
		return "claude-code/" + c.model
	}
	return "claude-code"
}

// Complete runs one generation.
//
// The user prompt goes on stdin rather than in an argument: it carries a
// reduced page, which runs to tens of kilobytes and would be an unreasonable
// thing to put in an argv.
//
// Tools are disabled explicitly. The CLI is an agent by default, and an agent
// asked to write an extractor might reasonably decide to go and fetch the page
// itself — which would mean the artifact was written against something other
// than the snapshot the harness fingerprinted and trialled it on.
func (c *ClaudeCode) Complete(ctx context.Context, system, user string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	args := []string{
		"--print",
		"--output-format", "text",
		"--allowed-tools", "",
		"--system-prompt", system,
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}

	command := exec.CommandContext(ctx, c.binary, args...)
	command.Stdin = strings.NewReader(user)

	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr

	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("claude timed out after %s: %w", c.timeout, ctx.Err())
		}
		return "", fmt.Errorf("claude failed: %w: %s", err, truncate(stderr.String(), maxErrorBody))
	}

	answer := strings.TrimSpace(stdout.String())
	if answer == "" {
		return "", fmt.Errorf("%w: claude printed nothing", ErrEmpty)
	}
	return answer, nil
}
