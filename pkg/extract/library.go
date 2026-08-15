package extract

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// Library is a set of pure functions a generated script may call.
//
// It exists because the same knowledge was being rediscovered in every
// artifact. Six extractors compiled for six museums each independently wrote a
// Swedish month table, a parser for "t.o.m." and "från", and a whitespace
// cleaner — hundreds of lines of the same thing, written slightly differently
// every time, each version a fresh opportunity to get a leap year or an
// en-dash wrong. That is the model paying for the same lesson repeatedly, and
// the operator reviewing a different version of it in every heal diff.
//
// Putting it here inverts that. A helper is written once, tested once, and
// improved once: adding a language to the shared date parser improves every
// extractor that has ever been generated, retroactively, without regenerating
// anything. This is where knowledge accumulates.
//
// Everything in a Library must be a pure function of its arguments. These are
// not capabilities and must never become them: a helper that fetched, wrote,
// or read a clock the caller did not supply would put back exactly what the
// sandbox exists to keep out.
type Library struct {
	// Global is the object the helpers hang off in JS.
	Global string

	// Helpers are the functions, in the order the prompt should list them.
	Helpers []Helper
}

// Helper is one function exposed to generated scripts.
type Helper struct {
	// Name is what a script calls it.
	Name string

	// Signature and Doc are what the model is told. They are the whole of the
	// model's knowledge of the helper, so they carry their weight in the
	// prompt: a helper the model does not understand is one it reimplements.
	Signature string
	Doc       string

	// Bind returns the Go function to install, given the page being extracted.
	// Taking the page allows a helper to answer questions about it — reading
	// its JSON-LD, resolving against its URL — without a script having to pass
	// it in.
	Bind func(page *Page, now time.Time) any
}

// Identity names the library and the helpers in it, for an artifact's
// provenance. Two libraries with the same helpers are interchangeable; one
// that has gained or lost a helper is not.
func (l *Library) Identity() string {
	if l == nil || len(l.Helpers) == 0 {
		return ""
	}
	names := make([]string, 0, len(l.Helpers))
	for _, helper := range l.Helpers {
		names = append(names, helper.Name)
	}
	slices.Sort(names)
	return l.Global + "{" + strings.Join(names, ",") + "}"
}

// Describe renders the library for the generation prompt.
func (l *Library) Describe() string {
	if l == nil || len(l.Helpers) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "A small standard library is available on the global %q. "+
		"Use it rather than reimplementing what it does — it is tested, it is "+
		"shared by every extractor, and it improves without them being "+
		"regenerated:\n\n", l.Global)

	for _, helper := range l.Helpers {
		fmt.Fprintf(&b, "  %s.%s\n      %s\n", l.Global, helper.Signature, helper.Doc)
	}
	return b.String()
}

// install builds the library object inside a runtime.
func (l *Library) install(vm *goja.Runtime, page *Page, now time.Time) error {
	if l == nil || len(l.Helpers) == 0 {
		return nil
	}

	object := vm.NewObject()
	for _, helper := range l.Helpers {
		if err := object.Set(helper.Name, guard(vm, helper.Name, helper.Bind(page, now))); err != nil {
			return fmt.Errorf("install %s.%s: %w", l.Global, helper.Name, err)
		}
	}
	return vm.Set(l.Global, object)
}

// guard wraps a helper so a panic inside it becomes a JavaScript exception
// rather than escaping into the host.
//
// The helpers are Go, and Go panics on an index out of range in a regexp
// group, a nil dereference, or a slice of a multi-byte string at the wrong
// offset. A panic raised while the interpreter is calling back into Go unwinds
// through goja and out of the sandbox, where the scheduler runs it in a bare
// goroutine and it takes the process down. That is the same defect the runner
// was fixed for, arriving through a different door, so the door is shut here
// too rather than trusted to stay closed.
func guard(vm *goja.Runtime, name string, fn any) func(goja.FunctionCall) goja.Value {
	callable, ok := goja.AssertFunction(vm.ToValue(fn))
	if !ok {
		panic(fmt.Sprintf("extract: helper %s is not callable", name))
	}

	return func(call goja.FunctionCall) (result goja.Value) {
		defer func() {
			if r := recover(); r != nil {
				// Rethrown as a JS TypeError: the script sees a failed call it
				// could catch, and the run is graded rather than lost.
				panic(vm.NewTypeError(fmt.Sprintf("%s failed: %v", name, r)))
			}
		}()

		returned, err := callable(goja.Undefined(), call.Arguments...)
		if err != nil {
			panic(vm.NewTypeError(fmt.Sprintf("%s failed: %v", name, err)))
		}
		return returned
	}
}
