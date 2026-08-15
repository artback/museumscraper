package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// serving starts a chat-completions server running handler, and returns a
// client pointed at it.
func serving(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	t.Setenv("EXTRACT_MODEL_ENDPOINT", server.URL+"/v1")
	t.Setenv("EXTRACT_MODEL", "test-model")

	client, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return client
}

func TestComplete(t *testing.T) {
	var request struct {
		Model       string  `json:"model"`
		Temperature float64 `json:"temperature"`
		Stream      bool    `json:"stream"`
		Messages    []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("posted to %s, want /v1/chat/completions", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]string{"role": "assistant", "content": `{"script":"ok"}`},
			}},
		})
	})

	got, err := client.Complete(context.Background(), "you are a compiler", "compile this")
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if got != `{"script":"ok"}` {
		t.Errorf("Complete() = %q, want the assistant's content", got)
	}

	// Sampling would make every regeneration produce a gratuitously different
	// script for an operator to review, so the temperature is pinned.
	if request.Temperature != 0 {
		t.Errorf("Complete() sent temperature %v, want 0", request.Temperature)
	}
	if request.Stream {
		t.Error("Complete() asked for a stream; it reads one whole answer")
	}
	if request.Model != "test-model" {
		t.Errorf("Complete() sent model %q, want %q", request.Model, "test-model")
	}
	if len(request.Messages) != 2 || request.Messages[0].Role != "system" {
		t.Errorf("Complete() sent %+v, want a system then a user message", request.Messages)
	}
}

func TestCompleteReportsServerErrors(t *testing.T) {
	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"model not loaded"}}`))
	})

	_, err := client.Complete(context.Background(), "s", "u")
	if err == nil {
		t.Fatal("Complete() returned no error for a 500")
	}
	// The body is quoted back, because "model not loaded" is the difference
	// between a bug and a missing `ollama pull`.
	if !strings.Contains(err.Error(), "model not loaded") {
		t.Errorf("Complete() error = %v, want the server's own message", err)
	}
}

func TestCompleteRejectsEmptyAnswers(t *testing.T) {
	client := serving(t, func(w http.ResponseWriter, _ *http.Request) {
		// What a local server does when the prompt overflowed its context: an
		// empty string and a finish reason, not an error.
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]string{"content": ""},
				"finish_reason": "length",
			}},
		})
	})

	_, err := client.Complete(context.Background(), "s", "u")
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("Complete() error = %v, want ErrEmpty", err)
	}
	if !strings.Contains(err.Error(), "length") {
		t.Errorf("Complete() error = %v, want the finish reason, which says the prompt did not fit", err)
	}
}

func TestNewRejectsBadEndpoint(t *testing.T) {
	t.Setenv("EXTRACT_MODEL_ENDPOINT", "localhost:11434")
	if _, err := New(); err == nil {
		t.Error("New() accepted an endpoint with no scheme")
	}

	t.Setenv("EXTRACT_MODEL_ENDPOINT", "http://localhost:11434/v1")
	t.Setenv("EXTRACT_MODEL_TIMEOUT", "not-a-duration")
	if _, err := New(); err == nil {
		t.Error("New() accepted an unparseable timeout")
	}
}

func TestNewDefaults(t *testing.T) {
	t.Setenv("EXTRACT_MODEL_ENDPOINT", "")
	t.Setenv("EXTRACT_MODEL", "")
	t.Setenv("EXTRACT_MODEL_TIMEOUT", "")

	client, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if client.Endpoint() != DefaultEndpoint {
		t.Errorf("Endpoint() = %q, want %q", client.Endpoint(), DefaultEndpoint)
	}
	if client.Name() != DefaultModel {
		t.Errorf("Name() = %q, want %q", client.Name(), DefaultModel)
	}
}

func TestCompleteSendsBearerToken(t *testing.T) {
	var authorization string
	client := serving(t, func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]string{"content": "ok"}}},
		})
	})

	// Set after the client was built, so rebuild it with the key in place.
	t.Setenv("EXTRACT_MODEL_KEY", "secret")
	keyed, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	keyed.endpoint = client.endpoint

	if _, err := keyed.Complete(context.Background(), "s", "u"); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if authorization != "Bearer secret" {
		t.Errorf("Authorization = %q, want %q", authorization, "Bearer secret")
	}
}
