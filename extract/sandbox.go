package extract

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dop251/goja"
)

// The contract every artifact obeys.
//
// A script defines one function, extract, which is handed the document and
// returns an array of plain objects. Requiring an array even from a source that
// yields a single record keeps the runner, the validator and the volumetric
// check from each needing a special case, and a model that returns a bare
// object is told so and gets it right on the retry.
const (
	// entryPoint is the function an artifact must define.
	entryPoint = "extract"

	// Contract is the description of that contract given to the model. It is
	// part of the prompt and therefore part of what Provenance pins down.
	Contract = `Define exactly one function:

    function extract(document) { ... }

It is called with the page's document and must return an array of plain
objects, one per record, using only the fields the schema declares.

Available on document: querySelector, querySelectorAll, getElementById,
getElementsByTagName, documentElement, body, title, URL.

Available on an element: querySelector, querySelectorAll, getAttribute,
hasAttribute, matches, closest, contains, tagName, id, className, classList,
textContent, innerText, innerHTML, outerHTML, href, src, attributes, dataset,
children, childElementCount, parentElement, firstElementChild,
lastElementChild, nextElementSibling, previousElementSibling.

href and src are already resolved to absolute URLs. innerText is whitespace
collapsed; textContent is not.

Beyond that and the standard library described below, nothing exists. There is
no fetch, no XMLHttpRequest, no require, no process, no timers, no DOM mutation
and no storage. Standard JavaScript built-ins (JSON, Math, String, Array,
RegExp, Date) are available.`
)

// Sandbox executes artifacts.
//
// It is safe for concurrent use and holds no per-run state: every Run builds a
// fresh interpreter, so one source's script cannot leave anything behind for
// the next. The cost of a new interpreter is microseconds against a fetch of
// tens of milliseconds.
type Sandbox struct {
	// Timeout bounds one execution. It is a wall-clock ceiling enforced by
	// interrupting the interpreter, which is the only bound goja offers: an
	// infinite loop is stopped, but the memory it allocated before being
	// stopped was still allocated, so the ceiling should stay tight.
	Timeout time.Duration

	// MaxRecords caps the returned array. A script returning more than this has
	// misunderstood the page rather than found a very long list.
	MaxRecords int

	// MaxCallStack bounds recursion, turning a runaway recursive walk into a
	// script error instead of a host stack overflow.
	MaxCallStack int

	// Library is the standard library exposed to scripts. Nil exposes none.
	//
	// It holds pure functions only. Passing one that is not pure would put a
	// capability back into an interpreter whose entire safety argument is that
	// it has none.
	Library *Library

	// Now supplies the instant the run is anchored to, captured once. Date
	// helpers resolve a listing that gives no year against it, so two runs at
	// different times can legitimately differ on such a listing — that is a
	// property of the data, not of the sandbox. Nil means time.Now.
	Now func() time.Time
}

// Sandbox defaults, all of them deliberately generous for honest scripts and
// tight for dishonest ones.
const (
	DefaultTimeout      = 5 * time.Second
	DefaultMaxRecords   = 5000
	DefaultMaxCallStack = 2048
)

// NewSandbox returns a Sandbox with the default ceilings.
func NewSandbox() *Sandbox {
	return &Sandbox{
		Timeout:      DefaultTimeout,
		MaxRecords:   DefaultMaxRecords,
		MaxCallStack: DefaultMaxCallStack,
	}
}

// Output is what one execution produced.
type Output struct {
	// Records is the extracted data, normalised to JSON scalars.
	Records []Record
	// Console is whatever the script logged, capped. It is fed back to the
	// model when a trial fails.
	Console string
	// Duration is how long execution took.
	Duration time.Duration
}

// Errors a run can fail with. They are distinguished because they call for
// different responses: a compile or runtime error is the artifact's fault and
// justifies healing, while a timeout may equally be a page that grew.
var (
	// ErrCompile means the script is not valid JavaScript.
	ErrCompile = errors.New("artifact does not compile")
	// ErrNoEntryPoint means the script compiled but defines no extract
	// function.
	ErrNoEntryPoint = errors.New("artifact defines no extract function")
	// ErrTimeout means execution exceeded the sandbox's ceiling.
	ErrTimeout = errors.New("artifact timed out")
	// ErrScript means the script threw.
	ErrScript = errors.New("artifact threw")
	// ErrShape means the script returned something other than an array of
	// objects.
	ErrShape = errors.New("artifact returned the wrong shape")
)

// Compile reports whether a script is valid JavaScript defining the entry
// point, without running it. The generator uses it to reject a hopeless
// candidate before paying for a page execution.
func Compile(script string) error {
	if _, err := goja.Compile("artifact.js", script, true); err != nil {
		return fmt.Errorf("%w: %w", ErrCompile, err)
	}
	return nil
}

// Run executes script against page.
//
// The interpreter it builds has a document, a console, and the JavaScript
// built-ins goja provides. It has no host bindings of any kind, so the
// isolation claim rests on what was never installed rather than on a filter
// that has to be right every time.
func (s *Sandbox) Run(ctx context.Context, script string, page *Page) (out Output, err error) {
	started := time.Now()

	program, compileErr := goja.Compile("artifact.js", script, true)
	if compileErr != nil {
		return Output{}, fmt.Errorf("%w: %w", ErrCompile, compileErr)
	}

	vm := goja.New()
	if s.MaxCallStack > 0 {
		vm.SetMaxCallStackSize(s.MaxCallStack)
	}

	binding, bindErr := newDOM(vm, page)
	if bindErr != nil {
		return Output{}, fmt.Errorf("build document: %w", bindErr)
	}

	// Captured once, so every helper in one run sees the same instant.
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	if err := s.Library.install(vm, page, now()); err != nil {
		return Output{}, fmt.Errorf("build library: %w", err)
	}

	stop := s.watch(ctx, vm)
	defer stop()

	finish := func(err error) (Output, error) {
		return Output{Console: binding.Output(), Duration: time.Since(started)}, err
	}

	// Nothing a script can do may leave this function as a panic.
	//
	// The interpreter signals a JS throw by panicking, and goja only recovers
	// that while it is running JS. Any value we touch afterwards can still run
	// JS — an accessor property on a returned object is invoked when the value
	// is exported — so a panic can surface here, outside every goja frame. The
	// runner is called from a bare goroutine in the scheduler, where a panic is
	// not recoverable and takes the process with it, so an artifact returning
	// `[{get title(){ return this.el.textContent }}]` would kill the host.
	//
	// vm.Try is not enough on its own: an interrupt and a stack overflow both
	// embed Exception by value rather than as a pointer, so goja's own
	// exceptionFromValue does not match them and re-panics. Hence a recover as
	// well, and hence routing it through classify — a timeout that lands during
	// export must still be reported as a timeout, or it is read as the
	// artifact's fault and drives a heal that fixes nothing.
	defer func() {
		if r := recover(); r != nil {
			out, err = finish(s.recovered(r))
		}
	}()

	if _, err := vm.RunProgram(program); err != nil {
		return finish(s.classify(err, ErrScript))
	}

	entry, ok := goja.AssertFunction(vm.Get(entryPoint))
	if !ok {
		return finish(fmt.Errorf("%w: no function named %q", ErrNoEntryPoint, entryPoint))
	}

	returned, runErr := entry(goja.Undefined(), vm.Get("document"))
	if runErr != nil {
		return finish(s.classify(runErr, ErrScript))
	}

	records, recordsErr := s.records(vm, returned)
	if recordsErr != nil {
		return finish(recordsErr)
	}

	out, _ = finish(nil)
	out.Records = records
	return out, nil
}

// recovered turns a panic that escaped the interpreter into an error.
func (s *Sandbox) recovered(r any) error {
	if err, ok := r.(error); ok {
		return s.classify(err, ErrScript)
	}
	return fmt.Errorf("%w: %v", ErrScript, r)
}

// watch arranges for the interpreter to be interrupted on timeout or on
// context cancellation, and returns a function that tears the watch down.
//
// Interrupt is the only goja call safe from another goroutine, which is why
// this is the shape it is: everything else touching the VM stays on the
// caller's goroutine.
func (s *Sandbox) watch(ctx context.Context, vm *goja.Runtime) func() {
	done := make(chan struct{})

	var timer *time.Timer
	if s.Timeout > 0 {
		timer = time.AfterFunc(s.Timeout, func() { vm.Interrupt(ErrTimeout) })
	}

	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt(context.Cause(ctx))
		case <-done:
		}
	}()

	return func() {
		close(done)
		if timer != nil {
			timer.Stop()
		}
		vm.ClearInterrupt()
	}
}

// classify turns a goja error into one of this package's, so a caller can tell
// a timeout from a thrown exception without matching on message text.
func (s *Sandbox) classify(err error, fallback error) error {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		switch cause := interrupted.Value().(type) {
		case error:
			return fmt.Errorf("%w after %s: %w", ErrTimeout, s.Timeout, cause)
		default:
			return fmt.Errorf("%w after %s: %v", ErrTimeout, s.Timeout, cause)
		}
	}

	var stack *goja.StackOverflowError
	if errors.As(err, &stack) {
		return fmt.Errorf("%w: stack overflow", ErrScript)
	}
	return fmt.Errorf("%w: %w", fallback, err)
}

// records converts the returned JS value into records, rejecting anything that
// is not an array of objects.
func (s *Sandbox) records(vm *goja.Runtime, value goja.Value) ([]Record, error) {
	if value == nil || goja.IsUndefined(value) || goja.IsNull(value) {
		return nil, fmt.Errorf("%w: returned %s, want an array", ErrShape, describe(value))
	}

	// The length is read before anything is exported, because exporting is what
	// allocates.
	//
	// `return new Array(1e9)` builds a sparse array in microseconds, so the
	// timeout has nothing to interrupt; Export then materialises it into a Go
	// slice at about sixteen bytes an element. Checking the cap on the exported
	// length enforced it only after the twenty-odd gigabytes had been asked
	// for, which on a Pi shared with sixteen other services is an OOM kill
	// rather than a rejected artifact.
	if object, ok := value.(*goja.Object); ok {
		var length int64
		if thrown := vm.Try(func() {
			// Get returns a nil Value, not undefined, for a property that is
			// not there — so a returned plain object has no length to read.
			if declared := object.Get("length"); declared != nil && !goja.IsUndefined(declared) {
				length = declared.ToInteger()
			}
		}); thrown != nil {
			return nil, fmt.Errorf("%w: reading the returned array's length threw: %v", ErrShape, thrown)
		}
		if max := int64(s.MaxRecords); max > 0 && length > max {
			return nil, fmt.Errorf("%w: returned %d records, over the limit of %d",
				ErrShape, length, max)
		}
	}

	// Exporting runs any accessor property the script defined, so it is guarded
	// too. A getter that throws would otherwise panic out of the sandbox.
	var raw any
	if thrown := vm.Try(func() { raw = value.Export() }); thrown != nil {
		return nil, fmt.Errorf("%w: exporting the result threw: %v", ErrScript, thrown)
	}

	exported, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: returned %s, want an array", ErrShape, describe(value))
	}
	if max := s.MaxRecords; max > 0 && len(exported) > max {
		return nil, fmt.Errorf("%w: returned %d records, over the limit of %d",
			ErrShape, len(exported), max)
	}

	records := make([]Record, 0, len(exported))
	for i, item := range exported {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: record %d is %T, want an object", ErrShape, i, item)
		}

		record := make(Record, len(fields))
		for name, raw := range fields {
			record[name] = normalize(raw)
		}
		records = append(records, record)
	}
	return records, nil
}

// normalize reduces an exported JS value to the JSON scalars a record may
// hold. Anything else is left as it is so the validator can reject it by name
// rather than having it silently stringified here.
func normalize(value any) any {
	switch v := value.(type) {
	case nil, string, bool, float64:
		return value
	case int64:
		// goja exports whole numbers as int64. Records are compared against
		// numeric rules and round-tripped through JSON, both of which are
		// simpler if every number is the same type.
		return float64(v)
	case int:
		return float64(v)
	case time.Time:
		// A script that built a Date exports one. Rendering it here means a
		// KindDate field arrives in the one format the validator parses first.
		return v.UTC().Format(time.RFC3339)
	default:
		return value
	}
}

// describe names a JS value's type for an error message.
func describe(value goja.Value) string {
	switch {
	case value == nil || goja.IsUndefined(value):
		return "undefined"
	case goja.IsNull(value):
		return "null"
	default:
		return value.ExportType().String()
	}
}
