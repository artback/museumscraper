/*
Package extract compiles a web page and a declared schema into durable,
executable extraction logic, and keeps that logic working as the page changes.

A language model reads a page once and writes a JavaScript extractor for it.
Every run after that executes the stored script with no model involved. The
model is a compiler, not a runtime, and is re-invoked only when a run has
produced evidence that the script has stopped working.

# The pieces

Each stage is usable on its own, and nothing here performs I/O: fetching pages
and storing artifacts are the caller's, so this package can be tested offline
and dropped into whatever transport and storage a program already has.

  - [ParsePage] parses a fetched page.
  - [Reducer] compresses it into a structural sketch small enough for a prompt.
  - [Generator] asks a [Model] for a script and trials it against that page.
  - [Sandbox] executes a stored script.
  - [Validator] grades the output pass, suspect or fail.
  - [Fingerprint] and [Similarity] compare pages structurally.
  - [ShouldHeal] and [HealPolicy] decide when regenerating is justified.

# Safety

Generated code is written by a model and must be treated as untrusted. It runs
in a bare goja interpreter holding a read-only DOM and nothing else: there is no
fetch, no XMLHttpRequest, no require, no process, no timers and no storage,
because none of them are ever installed. That is a property of an empty
interpreter rather than of a filter that has to be right every time.

Execution is bounded by a wall-clock interrupt, a cap on elements reached and a
cap on records returned, and no panic — including one raised by a getter on a
returned object — is allowed to escape [Sandbox.Run].

Two limits are worth knowing before relying on this. A regular expression with
catastrophic backtracking can run past the timeout, because goja routes some
patterns to an engine that does not poll its interrupt. And there is no
JavaScript renderer: a listing that exists only after client-side rendering is
invisible here.

# Validation

The point of grading every run is that an extractor which has silently stopped
working must not look like a source that has nothing to report. The rungs run
cheapest first and stop at the first definite failure:

 1. Structural — output present, parsing, conforming to the schema.
 2. Volumetric — a record count in character with this source's own history.
 3. Semantic — per-field rules the operator declared, never the model.
 4. Model-judged — optional, and the only rung that can cost an invocation.

The volumetric rung is the one that earns its keep: a page that drops from two
hundred rows to three returns three perfectly well-formed records, and
structural validity alone calls that a success.

# Extending it

[Library] adds pure functions to the interpreter for a script to call —
date parsing, text cleaning, whatever a domain repeatedly needs. Putting them
there rather than letting each generated script reinvent them means they are
written once, tested once, and improved once: a fix reaches every artifact
already generated, without regenerating any of them.

Everything in a [Library] must be a pure function of its arguments and the
page. A helper that fetched or wrote anything would put back exactly what the
sandbox exists to keep out.
*/
package extract
