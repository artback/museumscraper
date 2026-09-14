package useragent

import (
	"strings"
	"testing"
)

func TestForCarriesTheConfiguredContact(t *testing.T) {
	t.Setenv(ContactVar, "curator@example.org")

	agent := For("exhibition listings", "")
	for _, want := range []string{"museum-catalogue/", "curator@example.org", "exhibition listings"} {
		if !strings.Contains(agent, want) {
			t.Errorf("agent %q does not carry %q", agent, want)
		}
	}
}

// TestForNamesARealProject: the header must identify something an
// administrator can actually look up. Four clients previously named
// "github.com/example/museum", a placeholder that resolves to nothing, which
// Wikimedia's and Nominatim's policies both treat as no identification at all.
func TestForNamesARealProject(t *testing.T) {
	t.Setenv(ContactVar, "")

	agent := For("", "")
	if strings.Contains(agent, "example/museum") || strings.Contains(agent, "example.com") {
		t.Errorf("agent %q still names a placeholder", agent)
	}
	if !strings.Contains(agent, "github.com/artback/museumscraper") {
		t.Errorf("agent %q does not point anywhere real", agent)
	}
}

func TestForPrefersTheComponentOverrideThenTheGlobalOne(t *testing.T) {
	t.Setenv(AgentVar, "global/1.0")
	t.Setenv("TEST_LEGACY_AGENT", "legacy/1.0")

	if got := For("listings", "TEST_LEGACY_AGENT"); got != "legacy/1.0" {
		t.Errorf("with a component override set, got %q, want the override", got)
	}
	if got := For("listings", "TEST_UNSET_AGENT"); got != "global/1.0" {
		t.Errorf("with only the global override set, got %q, want it", got)
	}
}

// TestForIgnoresAnEmptyOverride: an override set to whitespace in a .env file
// must not produce a blank User-Agent, which is the one thing these services
// refuse outright.
func TestForIgnoresAnEmptyOverride(t *testing.T) {
	t.Setenv(AgentVar, "   ")
	t.Setenv(ContactVar, "curator@example.org")

	if agent := For("listings", ""); !strings.Contains(agent, "curator@example.org") {
		t.Errorf("agent %q, want the composed one", agent)
	}
}
