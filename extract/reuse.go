package extract

import (
	"hash/fnv"
	"slices"
)

// Reuse is the case for not generating at all.
//
// A corpus of museum websites is not a corpus of unique websites. Hundreds of
// them are the same WordPress theme, the same Squarespace template, the same
// regional agency's house CMS with a different logo — and an extractor written
// against one of those is written against all of them. The model is being paid
// to learn the same page twice.
//
// What makes this dangerous, and the reason it is gated the way it is, is
// measured rather than assumed. Every generated extractor was once run against
// every other museum's page: two of twenty cross-site pairs validated, and both
// were wrong — one extracted four records from a page whose own extractor finds
// twenty-four, and graded pass, because a first run has no volumetric baseline
// for anything in the ladder to notice the missing 83% against. Adding the
// standard library made it worse, not better: capable helpers let a generic
// extractor half-read a page it was never written for, and cross-site pairs
// that validated rose from 2 of 20 to 9 of 30, with record counts that matched
// the site's own extractor exactly while the records themselves were café
// opening hours.
//
// So validation is not the gate and cannot be made into one. Structural
// similarity is: the Jaccard index of two pages' path sets, which asks the only
// question that matters — are these the same page shape, so that the same
// selectors mean the same thing? Every unrelated pair of five real sites scored
// between 0.00 and 0.02.

// SketchSize is how many path hashes a Shape keeps.
//
// A bottom-k sketch estimates the Jaccard index with a standard error near
// 1/√k, so 128 puts the error around 8%. That is far finer than this decision
// needs — it separates 0.02 from 0.60 — and it keeps a Shape to about two
// kilobytes of JSON beside a script that is usually larger.
const SketchSize = 128

// ReuseThreshold is how alike two pages must be before one's extractor is
// tried on the other.
//
// Calibrated on real pages, where unrelated sites score 0.00–0.02 and a shared
// CMS theme scores high, so anything from 0.5 up is the same decision. It is
// set at the bottom of that range rather than higher because the trial and the
// validator still stand behind it: this gate exists to refuse the pairs that
// would pass validation while being wrong, and those are the ones scoring near
// zero.
const ReuseThreshold = 0.5

// Shape is a page's structural path set, compressed to something small enough
// to store on an artifact.
//
// A fingerprint answers "has this page changed" and is a hash, so it can answer
// nothing else. Reuse needs a different question — "how much of this page's
// structure does that page share" — and answering it from hashes means keeping
// something of the set itself. A page has thousands of distinct paths; the k
// smallest hashes of them are a uniform sample of the set that two pages agree
// on wherever their sets agree, which is exactly what makes the estimate work.
type Shape struct {
	// Paths is how many distinct structural paths the page had, kept because a
	// document with fewer than SketchSize of them is sampled exactly rather
	// than estimated, and because it is the first thing to look at when a
	// similarity score is surprising.
	Paths int `json:"paths"`
	// Min is the SketchSize smallest path hashes, ascending.
	Min []uint64 `json:"min"`
}

// ShapeOf sketches a page.
func ShapeOf(page *Page) Shape {
	if page == nil {
		return Shape{}
	}
	return shapeOfPaths(structuralPaths(page.doc))
}

func shapeOfPaths(paths map[string]struct{}) Shape {
	hashes := make([]uint64, 0, len(paths))
	for path := range paths {
		hash := fnv.New64a()
		_, _ = hash.Write([]byte(path))
		hashes = append(hashes, hash.Sum64())
	}

	slices.Sort(hashes)
	hashes = slices.Compact(hashes)

	shape := Shape{Paths: len(hashes)}
	shape.Min = hashes[:min(len(hashes), SketchSize)]
	return shape
}

// Empty reports that a shape says nothing — an artifact stored before shapes
// were recorded, or a page with no structure at all. Such a shape is never
// similar to anything, so an old artifact is simply not a candidate for reuse
// until the next time it is written.
func (s Shape) Empty() bool { return len(s.Min) == 0 }

// Similarity estimates the Jaccard index of the two pages the shapes came
// from, from 0 to 1.
//
// The estimator is the standard one for bottom-k sketches: take the k smallest
// hashes of the union, which is a uniform sample of it, and count how many of
// them are in both sets. Sampling the union rather than either side is what
// makes the answer symmetric and unbiased when the two pages differ wildly in
// size — a small page and a large one built from the same theme should not
// score differently depending on which is asked.
func (s Shape) Similarity(other Shape) float64 {
	if s.Empty() || other.Empty() {
		return 0
	}

	k := min(len(s.Min), len(other.Min))

	// The union's k smallest, walked in order from two ascending lists.
	var (
		shared, seen int
		i, j         int
	)
	for seen < k && (i < len(s.Min) || j < len(other.Min)) {
		switch {
		case j >= len(other.Min) || (i < len(s.Min) && s.Min[i] < other.Min[j]):
			// Present on the left only — but "only" is sound just here,
			// inside both sketches' range: past the end of the other side's
			// k smallest, absence is no longer evidence, which is what
			// stopping at k is for.
			i++
		case i >= len(s.Min) || other.Min[j] < s.Min[i]:
			j++
		default:
			shared++
			i, j = i+1, j+1
		}
		seen++
	}

	if seen == 0 {
		return 0
	}
	return float64(shared) / float64(seen)
}

// Match is a stored artifact judged similar enough to be worth trying on
// another site's page, and how alike the two pages are.
type Match struct {
	Artifact   Artifact
	Similarity float64
}

// MostSimilar ranks candidates by how much structure they share with a page,
// keeping only those above the threshold, most similar first.
//
// It returns an order rather than one answer because similarity is a claim
// about the pages and not about the script: the most similar page's extractor
// can still throw, or read the wrong thing, and the caller learns that by
// running it. What the threshold guarantees is that everything it hands back is
// worth the sandbox execution it costs — which is milliseconds, against the
// minutes the alternative costs.
func MostSimilar(shape Shape, candidates []Artifact, threshold float64) []Match {
	if shape.Empty() {
		return nil
	}

	matches := make([]Match, 0, len(candidates))
	for _, candidate := range candidates {
		similarity := shape.Similarity(candidate.Shape)
		if similarity < threshold {
			continue
		}
		matches = append(matches, Match{Artifact: candidate, Similarity: similarity})
	}

	slices.SortFunc(matches, func(a, b Match) int {
		switch {
		case a.Similarity > b.Similarity:
			return -1
		case a.Similarity < b.Similarity:
			return 1
		default:
			// Stable on the name, so the same corpus reuses the same artifact
			// twice running and an operator comparing two sites' provenance
			// sees one answer rather than a coin toss.
			return cmpString(a.Artifact.Source, b.Artifact.Source)
		}
	})
	return matches
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
