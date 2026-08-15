package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/html"
)

// fingerprintDepth is how many ancestors a structural path remembers.
//
// A full root-to-node path would change the moment a site wrapped its content
// in one more layout div, which happens constantly and breaks nothing. Four
// levels is enough to distinguish "a link inside a listing row" from "a link
// inside the footer", and shallow enough to survive the wrapper churn.
const fingerprintDepth = 4

// maxFingerprintPaths bounds the set a fingerprint is computed from, so a
// pathological page cannot make fingerprinting the expensive part of a run.
const maxFingerprintPaths = 20000

// Fingerprint is a structural signature of a page, insensitive to its content.
//
// It answers one question cheaply and without a model: has this page's shape
// changed since the artifact was written against it? That matters because a
// failing run means different things depending on the answer. Unchanged shape
// plus a failed run points at something transient or environmental, and
// regenerating would be spending a model invocation on a network blip.
// Changed shape plus a failed run is the case healing exists for.
//
// The signature is the set — not the list — of structural paths in the
// document. Taking the set is what makes it insensitive to content: a page
// with three listing rows and the same page with two hundred contain exactly
// the same distinct paths, so the fingerprint of a quiet month matches the
// fingerprint of a busy one. That is deliberate, and it is why the fingerprint
// cannot be used to detect a page emptying out. Counting rows is the
// validator's job, and it does it against this source's own history.
func Fingerprint(page *Page) string {
	if page == nil {
		return ""
	}
	return fingerprintNode(page.doc)
}

func fingerprintNode(root *html.Node) string {
	distinct := make([]string, 0, len(structuralPaths(root)))
	for path := range structuralPaths(root) {
		distinct = append(distinct, path)
	}
	slices.Sort(distinct)

	sum := sha256.Sum256([]byte(strings.Join(distinct, "\n")))
	return hex.EncodeToString(sum[:])
}

// structuralPaths is the set a fingerprint hashes and a similarity compares.
func structuralPaths(root *html.Node) map[string]struct{} {
	paths := make(map[string]struct{})
	var ancestry []string

	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node.Type == html.ElementNode {
			ancestry = append(ancestry, structuralShape(node))
			defer func() { ancestry = ancestry[:len(ancestry)-1] }()

			if len(paths) < maxFingerprintPaths {
				from := max(0, len(ancestry)-fingerprintDepth)
				paths[strings.Join(ancestry[from:], ">")] = struct{}{}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(root)
	return paths
}

// structuralShape is a node's contribution to a path: its tag and whatever of
// its classes look durable.
func structuralShape(node *html.Node) string {
	classes, _ := attribute(node, "class")

	var stable []string
	for _, token := range strings.Fields(classes) {
		if !volatileClass(token) {
			stable = append(stable, strings.ToLower(token))
		}
	}
	slices.Sort(stable)
	stable = slices.Compact(stable)

	if len(stable) == 0 {
		return node.Data
	}
	return node.Data + "." + strings.Join(stable, ".")
}

// generatedClass matches the class names build tools invent: CSS-module
// suffixes, styled-components identifiers, and content hashes. They change on
// every deploy without the layout changing at all, and including them would
// report drift on every rebuild of the site.
var generatedClass = regexp.MustCompile(
	`^(?:sc-[A-Za-z]{5,}|[A-Za-z]+__[A-Za-z]+___[A-Za-z0-9]{4,}|[A-Za-z-]+_[A-Za-z0-9]{5,}|[A-Za-z]*[0-9a-f]{8,})$`,
)

// stateClass names a class that tracks what the page is currently doing rather
// than how it is built. Which slide is active moves with a carousel; which nav
// item is current moves with the URL. Neither is a layout change.
var stateClass = regexp.MustCompile(
	`(^|-)(is|has)-|-(active|current|selected|open|visible|hidden|expanded|collapsed|loading|hover|focus|disabled)$`,
)

// volatileClass reports whether a class token should be left out of the
// fingerprint.
//
// This is a heuristic and will not catch every convention. Getting it wrong is
// survivable in one direction only, which is why it errs towards dropping:
// a class wrongly kept makes the fingerprint report drift that is not there,
// and since drift only ever changes how a failure is interpreted, the cost is
// a heal that was not needed. A class wrongly dropped costs a little
// sensitivity in a signal that is advisory to begin with.
func volatileClass(token string) bool {
	switch {
	case token == "":
		return true
	case generatedClass.MatchString(token):
		return true
	case stateClass.MatchString(strings.ToLower(token)):
		return true
	default:
		return false
	}
}

// Similarity is how much structure two pages share, from 0 to 1.
//
// It is the Jaccard index of their structural path sets — the same sets the
// fingerprint is a hash of. Two sites built on one CMS theme share most of
// their paths and score high; two unrelated sites score near zero.
//
// It exists to gate reuse of an extractor across sites. Validation alone is not
// a safe gate: an extractor written against one site was measured extracting
// four records from another and grading pass, where that site's own extractor
// found twenty-four. A first run has no volumetric baseline, so nothing in the
// ladder can see that eighty-three per cent of the page was missed. Structural
// similarity is the signal that distinguishes "the same page shape, so the same
// selectors mean the same thing" from "different page, and it happened to
// match something".
//
// Calibrate any threshold on real pages. The score is a fraction of the
// distinct paths in a document, and a small page has few of them, so the
// html/head/body skeleton every document shares is most of a synthetic
// fixture's score and almost none of a real one's. Measured across five real
// sites, every unrelated pair scored between 0.00 and 0.02 — so a gate
// anywhere from 0.5 upward is safe on real input, and no threshold at all is
// meaningful on a fixture of a dozen elements.
func Similarity(a, b *Page) float64 {
	if a == nil || b == nil {
		return 0
	}
	left, right := structuralPaths(a.doc), structuralPaths(b.doc)
	if len(left) == 0 || len(right) == 0 {
		return 0
	}

	shared := 0
	for path := range left {
		if _, ok := right[path]; ok {
			shared++
		}
	}

	union := len(left) + len(right) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

// Drifted reports whether two fingerprints differ. An empty fingerprint on
// either side means the question cannot be answered, and is reported as no
// drift so that a missing one never manufactures a reason to regenerate.
func Drifted(before, after string) bool {
	if before == "" || after == "" {
		return false
	}
	return before != after
}
