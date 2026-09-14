package harvest

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/artback/museumscraper/extract"
)

// Reuse is the harness's answer to a corpus that repeats itself.
//
// Museum websites are not bespoke. A regional agency builds twelve of them from
// one template, a country's museum association sells one WordPress theme to
// eighty members, and Squarespace's gallery layout is on hundreds. Generating an
// extractor for each is the model being paid, at minutes apiece on a Pi, to
// learn the same page over and over.
//
// The gate is structural similarity and never validation — see extract.Reuse
// for the measurement that settles this: cross-site extractors that validated
// were wrong, quietly, in the one way no rung of the ladder can catch on a first
// run. Similarity refuses exactly those pairs.

// MaxReuseTrials is how many stored artifacts are tried against a new page
// before giving up and generating.
//
// Each trial is a sandboxed execution — milliseconds — so the cost of trying is
// not what bounds this. What bounds it is that the list is ordered by
// similarity: if the three most structurally alike pages' extractors all fail on
// this page, the fourth is not a better theory, and generation is the answer.
const MaxReuseTrials = 3

// reuseCacheTTL is how long the candidate set is held between compiles.
//
// One refresh run compiles a handful of sources, and re-listing every stored
// artifact for each of them would make the saving cost a burst of object-storage
// requests. Newly compiled artifacts are added to the held set as they are made,
// which is the case that matters most: a run that meets five sites built from
// one template compiles the first and reuses it for the other four.
const reuseCacheTTL = 5 * time.Minute

// reuseCache holds the artifacts a reuse decision is made against.
type reuseCache struct {
	mu        sync.Mutex
	artifacts []extract.Artifact
	readAt    time.Time
}

// adopt gives a source an extractor written for a structurally identical site,
// reporting whether it found one that works.
//
// Three things have to hold, and the order is deliberate — cheapest first. The
// pages must be structurally alike, which costs an arithmetic comparison of two
// stored sketches. The script must then run on this page, which costs a
// sandboxed execution. And its output must pass the same validation a generated
// artifact's trial passes, against this source's own schema — so an artifact
// compiled for some other kind of source cannot be adopted merely for looking
// similar.
func (h *Harvester) adopt(ctx context.Context, source extract.Source, page *extract.Page) (extract.Artifact, extract.Report, bool) {
	shape := extract.ShapeOf(page)
	if shape.Empty() {
		return extract.Artifact{}, extract.Report{}, false
	}

	matches := extract.MostSimilar(shape, h.candidates(ctx, source.Name), extract.ReuseThreshold)
	if len(matches) == 0 {
		return extract.Artifact{}, extract.Report{}, false
	}

	for tried, match := range matches {
		if tried >= MaxReuseTrials || ctx.Err() != nil {
			break
		}

		// Graded as a first generation is: complete history, no baseline. There
		// is nothing to compare a first extraction against, and the volumetric
		// rung applies only the source's declared floor.
		//
		// This grading uses the runner's validator rather than a trial one, so
		// where the operator has enabled the model-judged rung it applies here
		// too — deliberately. That rung exists for output that survives every
		// cheap check and still does not answer the intent, which is exactly
		// the way a borrowed extractor fails: the café opening hours of the
		// cross-site measurement were structurally perfect records.
		assessment := h.execute(ctx, source, match.Artifact.Script, page, extract.History{Complete: true})
		if assessment.Verdict != extract.Pass {
			log.Printf("harvest: %s is %.2f alike %s but its extractor graded %s here: %s",
				source.Name, match.Similarity, match.Artifact.Source, assessment.Verdict,
				firstOr(assessment.Findings, "no reason recorded"))
			continue
		}

		now := h.now()
		artifact := extract.Artifact{
			Source:      source.Name,
			Version:     1,
			Script:      match.Artifact.Script,
			Fingerprint: extract.Fingerprint(page),
			Shape:       shape,
			Provenance: extract.Provenance{
				// Named for what wrote the script, which is the model that
				// wrote it for the other site. An operator asking "what
				// produced this" gets the same answer they would get there,
				// plus the fact that it arrived by reuse.
				Model:       match.Artifact.Provenance.Model,
				Prompt:      match.Artifact.Provenance.Prompt,
				PageDigest:  extract.Digest(page.HTML),
				Library:     h.sandbox().Library.Identity(),
				ReusedFrom:  match.Artifact.Source,
				Similarity:  match.Similarity,
				GeneratedAt: now,
			},
			CreatedAt: now,
		}

		log.Printf("harvest: %s adopted %s's extractor (%.2f alike, %d records) — nothing generated",
			source.Name, match.Artifact.Source, match.Similarity, len(assessment.Records))

		return artifact, extract.Report{
			Reused:     match.Artifact.Source,
			Similarity: match.Similarity,
			Tried:      tried + 1,
		}, true
	}

	return extract.Artifact{}, extract.Report{}, false
}

// candidates is every other source's current artifact, excluding the source
// being compiled and any source a human has had to pause.
//
// A quarantined source is one whose extractor stopped working and could not be
// repaired. Whatever is wrong with it — a script that throws, a site that
// changed under it — is not a thing to propagate to a second site on the
// strength of the two looking alike.
func (h *Harvester) candidates(ctx context.Context, exclude string) []extract.Artifact {
	h.reuse.mu.Lock()
	defer h.reuse.mu.Unlock()

	if h.reuse.readAt.IsZero() || h.now().Sub(h.reuse.readAt) > reuseCacheTTL {
		artifacts, err := h.Store.CurrentArtifacts(ctx)
		if err != nil {
			// Not being able to look is not a reason to fail a compile. It
			// means this source is generated rather than adopted, which is what
			// would have happened anyway before there was anything to adopt.
			log.Printf("harvest: could not read stored artifacts to look for a reusable one: %v", err)
			return nil
		}

		paused := h.pausedSources(ctx)
		h.reuse.artifacts = h.reuse.artifacts[:0]
		for _, artifact := range artifacts {
			if paused[artifact.Source] || artifact.Shape.Empty() {
				continue
			}
			h.reuse.artifacts = append(h.reuse.artifacts, artifact)
		}
		h.reuse.readAt = h.now()
	}

	held := make([]extract.Artifact, 0, len(h.reuse.artifacts))
	for _, artifact := range h.reuse.artifacts {
		if artifact.Source == exclude {
			continue
		}
		held = append(held, artifact)
	}
	return held
}

// pausedSources names the sources that must not be reused from. A store that
// cannot be listed yields none, which errs towards not reusing: an unknown
// paused set with an empty candidate list reuses nothing at all.
func (h *Harvester) pausedSources(ctx context.Context) map[string]bool {
	sources, err := h.Store.Sources(ctx)
	if err != nil {
		log.Printf("harvest: could not read sources while looking for a reusable extractor: %v", err)
		return nil
	}

	paused := make(map[string]bool)
	for _, source := range sources {
		if source.Paused {
			paused[source.Name] = true
		}
	}
	return paused
}

// remember adds an artifact compiled in this process to the candidate set, so
// the second site built from one template does not have to wait for the cache
// to expire to benefit from the first.
func (h *Harvester) remember(artifact extract.Artifact) {
	if artifact.Shape.Empty() {
		return
	}

	h.reuse.mu.Lock()
	defer h.reuse.mu.Unlock()

	if h.reuse.readAt.IsZero() {
		// Nothing has been read yet, so there is no set to add to; the first
		// read will pick this artifact up from the store.
		return
	}

	// Replacing rather than appending, because a healed artifact supersedes the
	// one that stopped working and offering the broken one to a sibling is the
	// one way this could spread a fault.
	for i, held := range h.reuse.artifacts {
		if held.Source == artifact.Source {
			h.reuse.artifacts[i] = artifact
			return
		}
	}
	h.reuse.artifacts = append(h.reuse.artifacts, artifact)
}
