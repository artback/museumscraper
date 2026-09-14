// Package useragent builds the User-Agent header every outbound request
// carries, from one definition.
//
// Being identifiable is not politeness here, it is the condition of access.
// Wikimedia's user-agent policy refuses requests from clients it cannot
// attribute, Nominatim's usage policy requires an agent naming the application
// and a way to reach whoever runs it, and Overpass asks the same of automated
// clients. Each of the four clients in this repository used to carry its own
// constant, and all four named "github.com/example/museum" — an address that
// does not exist, which is the same as no contact at all and grounds for a
// block that arrives as an opaque 403 weeks later.
//
// One definition also makes the contact detail a deployment concern rather
// than a source change: MUSEUM_CONTACT is the one variable an operator has to
// set, and every source sees it.
package useragent

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

const (
	// product and version identify this software to the services it reads.
	product = "museum-catalogue"
	version = "1.0"

	// projectURL is where the crawler is documented. It stands in for a
	// contact when none is configured: a real repository an administrator can
	// open beats a placeholder domain, though it is no substitute for an
	// address that reaches a person.
	projectURL = "https://github.com/artback/museumscraper"

	// ContactVar names the variable holding the operator's contact detail — an
	// email address or a URL, whichever the operator prefers to be reached at.
	ContactVar = "MUSEUM_CONTACT"

	// AgentVar overrides the whole header, for a deployment that would rather
	// state its own string than have one composed.
	AgentVar = "MUSEUM_USER_AGENT"
)

// warnOnce keeps the missing-contact warning to one line per process rather
// than one per client constructed.
var warnOnce sync.Once

// For returns the User-Agent for one component.
//
// purpose is a short phrase describing what this component reads ("exhibition
// listings"), carried so an administrator reading their logs can tell which
// part of the crawler they are looking at. legacyVar names the
// component-specific variable that used to configure this client, honoured
// ahead of everything else so existing deployments keep working; pass "" where
// there is none.
func For(purpose, legacyVar string) string {
	if legacyVar != "" {
		if agent := strings.TrimSpace(os.Getenv(legacyVar)); agent != "" {
			return agent
		}
	}
	if agent := strings.TrimSpace(os.Getenv(AgentVar)); agent != "" {
		return agent
	}

	details := []string{"+" + projectURL}
	if contact := Contact(); contact != "" {
		details = append(details, contact)
	} else {
		warnOnce.Do(func() {
			log.Printf("useragent: %s is not set, so outbound requests carry no contact address. "+
				"Wikimedia and Nominatim both ask for one and may refuse or block a crawler without it; "+
				"set %s to an email address or a URL that reaches you.", ContactVar, ContactVar)
		})
	}
	if purpose != "" {
		details = append(details, purpose)
	}

	return fmt.Sprintf("%s/%s (%s)", product, version, strings.Join(details, "; "))
}

// Contact returns the configured contact detail, or "" when the operator has
// not supplied one.
func Contact() string {
	return strings.TrimSpace(os.Getenv(ContactVar))
}
