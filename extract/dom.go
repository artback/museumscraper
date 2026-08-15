package extract

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/andybalholm/cascadia"
	"github.com/dop251/goja"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Page is a fetched page, parsed once and reused.
//
// The parse is shared between the reducer, the fingerprinter and the sandbox
// because all three want the same tree and parsing a large listing page is the
// most expensive thing any of them does.
type Page struct {
	// URL is where the page ended up after redirects. Links are resolved
	// against it rather than against the URL that was requested, which is what
	// a browser does and what the difference between the two costs when a site
	// redirects /listings to /en/listings.
	URL string
	// HTML is the page as fetched.
	HTML string

	doc  *html.Node
	base *url.URL
}

// maxNestingDepth bounds how deeply a page may nest open tags.
//
// html.Parse is quadratic in nesting depth — measured at 0.4s for ten thousand
// nested divs, 2.4s for twenty-five thousand and 9s for fifty thousand — and it
// takes no context and no deadline, so nothing can interrupt it. A body cap of
// four megabytes admits hundreds of thousands of nested tags, which is tens of
// minutes inside a call the harness cannot abandon: the source's slot in the
// scheduler is held for the whole of it and never released.
//
// Real pages are nowhere near this: measured markup rarely passes thirty
// levels, and the reducer stops descending at eighteen.
const maxNestingDepth = 512

// ErrTooDeep means a page nests markup far past anything a real site produces.
var ErrTooDeep = errors.New("page nests markup too deeply to parse safely")

// ParsePage parses a fetched page.
func ParsePage(pageURL, body string) (*Page, error) {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, fmt.Errorf("parse page URL %q: %w", pageURL, err)
	}

	// Checked before parsing, because the parse is what cannot be interrupted.
	if depth := nestingDepth(body); depth > maxNestingDepth {
		return nil, fmt.Errorf("%w: %s nests %d levels, over the limit of %d",
			ErrTooDeep, pageURL, depth, maxNestingDepth)
	}

	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", pageURL, err)
	}
	return &Page{URL: pageURL, HTML: body, doc: doc, base: base}, nil
}

// nestingDepth is a cheap upper bound on how deeply a document nests.
//
// It counts opening against closing tags in a single pass over the text rather
// than parsing, which is the whole point: it has to be cheaper than the thing
// it is protecting. It over-counts on unbalanced markup, which is the safe
// direction — the limit is an order of magnitude above any real page.
func nestingDepth(body string) int {
	var depth, deepest int

	for i := 0; i < len(body); i++ {
		if body[i] != '<' || i+1 >= len(body) {
			continue
		}
		switch next := body[i+1]; {
		case next == '/':
			if depth > 0 {
				depth--
			}
		case next == '!' || next == '?':
			// Comments, doctypes and processing instructions nest nothing.
		case isTagStart(next):
			// A self-closing tag opens and shuts in one go; void elements are
			// left to over-count, which the limit has room for.
			if end := strings.IndexByte(body[i:], '>'); end > 1 && body[i+end-1] == '/' {
				continue
			}
			depth++
			if depth > deepest {
				deepest = depth
			}
		}
	}
	return deepest
}

func isTagStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// Root returns the parsed document node.
func (p *Page) Root() *html.Node { return p.doc }

// resolve turns a possibly relative reference into an absolute URL, leaving it
// alone when it cannot be parsed.
func (p *Page) resolve(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || p.base == nil {
		return ref
	}
	parsed, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return p.base.ResolveReference(parsed).String()
}

// DOM ceilings. A generated script is not trusted to be reasonable, so the
// shapes it can ask for are bounded. These are far above what an honest
// extraction of a listing page needs and far below what would exhaust memory.
const (
	// maxQueryResults caps one querySelectorAll. A selector matching more than
	// this is not extracting a listing, it is selecting the whole document.
	maxQueryResults = 20000

	// maxLiveNodes caps how many distinct elements one execution may reach.
	//
	// Derived from the measured cost of a wrapped node rather than guessed. A
	// node was ~26 KB when every element carried its own copy of the interface;
	// sharing the prototype took it to ~1 KB, so this ceiling is about 50 MB of
	// wrappers rather than the 5 GB the old constant permitted. A listing page
	// with more than fifty thousand reachable elements is pathological, and
	// stopping is better than being OOM-killed on a Pi shared with sixteen
	// other services.
	maxLiveNodes = 50000

	// maxSelectorCache caps the compiled-selector cache, so a script building
	// selectors in a loop cannot grow it without bound.
	maxSelectorCache = 512

	// maxConsoleBytes caps captured console output. The output is fed back to
	// the model when a trial fails, so it needs to be small enough to sit in a
	// prompt.
	maxConsoleBytes = 4096
)

// dom binds a parsed page into a goja runtime as a read-only document.
//
// Every object it creates is a plain goja object with Go functions attached.
// There is no reflection over Go values into the VM, so the script cannot reach
// a Go type's method set or any field that was not deliberately exposed here.
type dom struct {
	vm   *goja.Runtime
	page *Page

	// wrapped caches one JS object per HTML node, so that a node reached twice
	// is the same object in JS. Scripts compare elements with ===, and a fresh
	// object per traversal would make every such comparison false.
	wrapped map[*html.Node]*goja.Object

	// nodes is the reverse mapping, for turning an element handed back from
	// JS into the node behind it. Scanning wrapped for it instead would make
	// a script that calls contains() once per row quadratic in the size of
	// the page.
	nodes map[*goja.Object]*html.Node

	// elementProto carries every element method and property, defined once.
	// They used to be installed per node — twenty-nine fresh closures each —
	// which cost about 26 KB per element and made maxLiveNodes a licence to
	// allocate gigabytes.
	elementProto *goja.Object

	// childArrays caches the JS array behind .children, keyed by node.
	//
	// Without it, `for (i < c.childElementCount) c.children[i]` — the shape a
	// model reaches for by reflex — rebuilds an n-element array on every
	// iteration and is quadratic: measured at 30, 92 and 364 MiB for 500,
	// 1,000 and 2,000 rows. Caching is safe because the tree is read-only for
	// the whole execution, and it matches the DOM, where .children is one live
	// collection rather than a fresh list per access.
	childArrays map[*html.Node]goja.Value

	// childNodes caches the element children of a node. childElementCount and
	// the first/last accessors each rebuilt this slice on every access, so a
	// loop bounded by childElementCount stayed quadratic even once the JS
	// array behind .children was cached.
	childNodes map[*html.Node][]*html.Node

	// selectors caches compiled selectors, which dominate the cost of a script
	// that queries inside a loop over rows.
	selectors map[string]cascadia.Selector

	console strings.Builder
}

// newDOM installs document and console into vm and returns the binding.
func newDOM(vm *goja.Runtime, page *Page) (*dom, error) {
	d := &dom{
		vm:          vm,
		page:        page,
		wrapped:     make(map[*html.Node]*goja.Object),
		nodes:       make(map[*goja.Object]*html.Node),
		childArrays: make(map[*html.Node]goja.Value),
		childNodes:  make(map[*html.Node][]*html.Node),
		selectors:   make(map[string]cascadia.Selector),
	}
	d.elementProto = d.buildElementProto()

	document := vm.NewObject()
	d.attachQueries(document, page.doc)

	if err := document.Set("getElementById", d.getElementByID); err != nil {
		return nil, err
	}
	d.defineGetter(document, "documentElement", func() goja.Value {
		return d.wrap(findElement(page.doc, atom.Html))
	})
	d.defineGetter(document, "body", func() goja.Value {
		return d.wrap(findElement(page.doc, atom.Body))
	})
	d.defineGetter(document, "title", func() goja.Value {
		return vm.ToValue(collapse(textOf(findElement(page.doc, atom.Title))))
	})
	// Both spellings, because scripts reach for either and a ReferenceError on
	// a page's own address is a silly way to lose an otherwise good artifact.
	for _, name := range []string{"URL", "baseURI"} {
		d.defineGetter(document, name, func() goja.Value { return vm.ToValue(page.URL) })
	}

	if err := vm.Set("document", document); err != nil {
		return nil, err
	}

	// console exists, captures, and does nothing else. It is here because
	// models write console.log by reflex: without it an otherwise correct
	// script dies on a ReferenceError, and the generator burns a retry on a
	// debugging statement. What it captures is shown to the model when a trial
	// fails, which makes the reflex useful instead of merely harmless.
	console := vm.NewObject()
	for _, level := range []string{"log", "warn", "error", "info", "debug"} {
		if err := console.Set(level, d.log); err != nil {
			return nil, err
		}
	}
	if err := vm.Set("console", console); err != nil {
		return nil, err
	}
	return d, nil
}

// Output returns whatever the script wrote to console.
func (d *dom) Output() string { return d.console.String() }

func (d *dom) log(call goja.FunctionCall) goja.Value {
	if d.console.Len() >= maxConsoleBytes {
		return goja.Undefined()
	}
	parts := make([]string, 0, len(call.Arguments))
	for _, arg := range call.Arguments {
		parts = append(parts, arg.String())
	}
	line := strings.Join(parts, " ")
	if room := maxConsoleBytes - d.console.Len(); len(line) > room {
		line = line[:room]
	}
	d.console.WriteString(line)
	d.console.WriteByte('\n')
	return goja.Undefined()
}

// throw raises a JS exception. goja turns a panic with a JS value into a
// throw in the running script, so a script that asks for something impossible
// fails in JavaScript terms rather than taking the host down.
func (d *dom) throw(format string, args ...any) {
	panic(d.vm.NewTypeError(fmt.Sprintf(format, args...)))
}

// compile returns a compiled selector, caching it.
func (d *dom) compile(selector string) cascadia.Selector {
	if compiled, ok := d.selectors[selector]; ok {
		return compiled
	}
	compiled, err := cascadia.Compile(selector)
	if err != nil {
		d.throw("invalid selector %q: %v", selector, err)
	}
	if len(d.selectors) < maxSelectorCache {
		d.selectors[selector] = compiled
	}
	return compiled
}

// attachQueries gives an object the two query methods, rooted at node.
func (d *dom) attachQueries(obj *goja.Object, node *html.Node) {
	obj.Set("querySelector", func(selector string) goja.Value {
		return d.wrap(cascadia.Query(node, d.compile(selector)))
	})
	obj.Set("querySelectorAll", func(selector string) goja.Value {
		return d.queryAll(node, selector)
	})
	// getElementsByTagName is old but common in generated code. It shares
	// queryAll so that it cannot drift out of step with the result ceiling,
	// which it had: it was written without one.
	obj.Set("getElementsByTagName", func(name string) goja.Value {
		return d.queryAll(node, name)
	})
}

func (d *dom) getElementByID(id string) goja.Value {
	// Escaped through a selector rather than by walking the tree, so that an
	// id with a colon or a dot in it — Drupal and Vue both emit them — cannot
	// be read as a selector.
	return d.wrap(cascadia.Query(d.page.doc, d.compile("[id="+quoteSelectorValue(id)+"]")))
}

// wrapAll wraps a slice of nodes into a JS array.
func (d *dom) wrapAll(nodes []*html.Node) goja.Value {
	items := make([]any, 0, len(nodes))
	for _, n := range nodes {
		items = append(items, d.wrap(n))
	}
	return d.vm.NewArray(items...)
}

// wrap returns the JS object for a node, creating it on first reach. A nil
// node becomes JS null, which is what the DOM returns for a query that found
// nothing.
//
// The object itself is empty: every method and property lives on the shared
// prototype, so reaching a node costs one object and a prototype link rather
// than twenty-nine closures.
func (d *dom) wrap(node *html.Node) goja.Value {
	if node == nil {
		return goja.Null()
	}
	if existing, ok := d.wrapped[node]; ok {
		return existing
	}
	if len(d.wrapped) >= maxLiveNodes {
		d.throw("script reached more than %d elements", maxLiveNodes)
	}

	obj := d.vm.NewObject()
	if err := obj.SetPrototype(d.elementProto); err != nil {
		d.throw("could not build element: %v", err)
	}
	d.wrapped[node] = obj
	d.nodes[obj] = node
	return obj
}

// self recovers the node a prototype method was invoked on.
func (d *dom) self(call goja.FunctionCall) *html.Node {
	obj, ok := call.This.(*goja.Object)
	if !ok {
		d.throw("element method called without an element")
	}
	node, ok := d.nodes[obj]
	if !ok {
		d.throw("element method called on something that is not an element")
	}
	return node
}

// method installs a function on the prototype.
func (d *dom) method(proto *goja.Object, name string, fn func(node *html.Node, call goja.FunctionCall) goja.Value) {
	if err := proto.Set(name, func(call goja.FunctionCall) goja.Value {
		return fn(d.self(call), call)
	}); err != nil {
		panic(fmt.Sprintf("extract: install %s: %v", name, err))
	}
}

// property installs a lazily evaluated read-only property on the prototype.
//
// Lazily, because a page has tens of thousands of nodes and computing every
// node's textContent, innerHTML and dataset up front dwarfs the extraction.
// Read-only, because the DOM handed to a generated script is a view of a
// fetched page, and a script that appeared to mutate it would be writing to
// something no one will ever read.
func (d *dom) property(proto *goja.Object, name string, get func(node *html.Node) goja.Value) {
	getter := d.vm.ToValue(func(call goja.FunctionCall) goja.Value {
		return get(d.self(call))
	})
	if err := proto.DefineAccessorProperty(name, getter, nil, goja.FLAG_FALSE, goja.FLAG_TRUE); err != nil {
		panic(fmt.Sprintf("extract: define %s: %v", name, err))
	}
}

// buildElementProto defines the element interface once per execution.
func (d *dom) buildElementProto() *goja.Object {
	proto := d.vm.NewObject()

	d.method(proto, "querySelector", func(node *html.Node, call goja.FunctionCall) goja.Value {
		return d.wrap(cascadia.Query(node, d.compile(call.Argument(0).String())))
	})
	d.method(proto, "querySelectorAll", func(node *html.Node, call goja.FunctionCall) goja.Value {
		return d.queryAll(node, call.Argument(0).String())
	})
	d.method(proto, "getElementsByTagName", func(node *html.Node, call goja.FunctionCall) goja.Value {
		return d.queryAll(node, call.Argument(0).String())
	})

	d.method(proto, "getAttribute", func(node *html.Node, call goja.FunctionCall) goja.Value {
		if value, ok := attribute(node, call.Argument(0).String()); ok {
			return d.vm.ToValue(value)
		}
		return goja.Null()
	})
	d.method(proto, "hasAttribute", func(node *html.Node, call goja.FunctionCall) goja.Value {
		_, ok := attribute(node, call.Argument(0).String())
		return d.vm.ToValue(ok)
	})
	d.method(proto, "matches", func(node *html.Node, call goja.FunctionCall) goja.Value {
		return d.vm.ToValue(d.compile(call.Argument(0).String()).Match(node))
	})
	d.method(proto, "closest", func(node *html.Node, call goja.FunctionCall) goja.Value {
		compiled := d.compile(call.Argument(0).String())
		for n := node; n != nil; n = n.Parent {
			if n.Type == html.ElementNode && compiled.Match(n) {
				return d.wrap(n)
			}
		}
		return goja.Null()
	})
	d.method(proto, "contains", func(node *html.Node, call goja.FunctionCall) goja.Value {
		target := d.unwrap(call.Argument(0))
		for n := target; n != nil; n = n.Parent {
			if n == node {
				return d.vm.ToValue(true)
			}
		}
		return d.vm.ToValue(false)
	})

	d.property(proto, "tagName", func(node *html.Node) goja.Value {
		return d.vm.ToValue(strings.ToUpper(node.Data))
	})
	d.property(proto, "localName", func(node *html.Node) goja.Value { return d.vm.ToValue(node.Data) })
	d.property(proto, "id", func(node *html.Node) goja.Value {
		value, _ := attribute(node, "id")
		return d.vm.ToValue(value)
	})
	d.property(proto, "className", func(node *html.Node) goja.Value {
		value, _ := attribute(node, "class")
		return d.vm.ToValue(value)
	})
	d.property(proto, "classList", func(node *html.Node) goja.Value {
		value, _ := attribute(node, "class")
		return d.vm.ToValue(strings.Fields(value))
	})

	// textContent is the DOM\'s: every descendant\'s text, concatenated, with
	// nothing normalised. innerText is the collapsed, script-and-style-free
	// version a human would read. Both exist because both get written, and a
	// script that reaches for the wrong one on a page full of newlines produces
	// titles that fail validation for reasons nobody can see.
	d.property(proto, "textContent", func(node *html.Node) goja.Value {
		return d.vm.ToValue(textOf(node))
	})
	for _, name := range []string{"innerText", "text"} {
		d.property(proto, name, func(node *html.Node) goja.Value {
			return d.vm.ToValue(collapse(visibleTextOf(node)))
		})
	}
	d.property(proto, "innerHTML", func(node *html.Node) goja.Value { return d.vm.ToValue(innerHTML(node)) })
	d.property(proto, "outerHTML", func(node *html.Node) goja.Value { return d.vm.ToValue(outerHTML(node)) })

	// href and src are resolved against the page, as they are in a browser. The
	// raw attribute remains reachable through getAttribute, which is the
	// distinction the DOM itself draws.
	for _, name := range []string{"href", "src"} {
		d.property(proto, name, func(node *html.Node) goja.Value {
			value, ok := attribute(node, name)
			if !ok {
				return d.vm.ToValue("")
			}
			return d.vm.ToValue(d.page.resolve(value))
		})
	}

	d.property(proto, "attributes", func(node *html.Node) goja.Value {
		attrs := make(map[string]any, len(node.Attr))
		for _, a := range node.Attr {
			attrs[a.Key] = a.Val
		}
		return d.vm.ToValue(attrs)
	})
	d.property(proto, "dataset", func(node *html.Node) goja.Value {
		data := make(map[string]any)
		for _, a := range node.Attr {
			if name, ok := strings.CutPrefix(a.Key, "data-"); ok {
				data[camel(name)] = a.Val
			}
		}
		return d.vm.ToValue(data)
	})

	d.property(proto, "children", d.children)
	d.property(proto, "childElementCount", func(node *html.Node) goja.Value {
		return d.vm.ToValue(len(d.childElements(node)))
	})
	d.property(proto, "parentElement", func(node *html.Node) goja.Value {
		if node.Parent != nil && node.Parent.Type == html.ElementNode {
			return d.wrap(node.Parent)
		}
		return goja.Null()
	})
	d.property(proto, "firstElementChild", func(node *html.Node) goja.Value {
		if kids := d.childElements(node); len(kids) > 0 {
			return d.wrap(kids[0])
		}
		return goja.Null()
	})
	d.property(proto, "lastElementChild", func(node *html.Node) goja.Value {
		if kids := d.childElements(node); len(kids) > 0 {
			return d.wrap(kids[len(kids)-1])
		}
		return goja.Null()
	})
	d.property(proto, "nextElementSibling", func(node *html.Node) goja.Value {
		return d.wrap(siblingElement(node, forward))
	})
	d.property(proto, "previousElementSibling", func(node *html.Node) goja.Value {
		return d.wrap(siblingElement(node, backward))
	})

	return proto
}

// queryAll runs a selector and enforces the result ceiling.
//
// querySelectorAll and getElementsByTagName share it so that the cap cannot be
// present on one and missing on the other, which is what happened when they
// were written separately.
func (d *dom) queryAll(node *html.Node, selector string) goja.Value {
	found := cascadia.QueryAll(node, d.compile(selector))
	if len(found) > maxQueryResults {
		d.throw("selector %q matched %d elements, over the limit of %d",
			selector, len(found), maxQueryResults)
	}
	return d.wrapAll(found)
}

// children returns the cached child collection for a node.
func (d *dom) children(node *html.Node) goja.Value {
	if cached, ok := d.childArrays[node]; ok {
		return cached
	}
	array := d.wrapAll(d.childElements(node))
	d.childArrays[node] = array
	return array
}

// childElements returns a node's element children, cached.
//
// The cache cannot go stale: the tree is read-only for the whole execution,
// which is the same invariant that lets .children be handed out as one live
// collection.
func (d *dom) childElements(node *html.Node) []*html.Node {
	if cached, ok := d.childNodes[node]; ok {
		return cached
	}
	kids := childElements(node)
	d.childNodes[node] = kids
	return kids
}

// unwrap recovers the HTML node behind a JS element object, or nil for
// anything this binding did not create.
func (d *dom) unwrap(value goja.Value) *html.Node {
	obj, ok := value.(*goja.Object)
	if !ok {
		return nil
	}
	return d.nodes[obj]
}

// defineGetter installs a lazily evaluated read-only property on the document.
//
// Elements use property() against the shared prototype instead; this remains
// for the document, of which there is exactly one, so its closures do not
// scale with the page.
func (d *dom) defineGetter(obj *goja.Object, name string, get func() goja.Value) {
	getter := d.vm.ToValue(func(goja.FunctionCall) goja.Value { return get() })
	if err := obj.DefineAccessorProperty(name, getter, nil, goja.FLAG_FALSE, goja.FLAG_TRUE); err != nil {
		// Only a malformed property name reaches this, and every name here is
		// a constant, so it is a programming error rather than a runtime one.
		panic(fmt.Sprintf("extract: define %s: %v", name, err))
	}
}

// direction is which way siblingElement walks.
type direction bool

const (
	forward  direction = true
	backward direction = false
)

func siblingElement(node *html.Node, dir direction) *html.Node {
	step := func(n *html.Node) *html.Node {
		if dir == forward {
			return n.NextSibling
		}
		return n.PrevSibling
	}
	for n := step(node); n != nil; n = step(n) {
		if n.Type == html.ElementNode {
			return n
		}
	}
	return nil
}

func childElements(node *html.Node) []*html.Node {
	var kids []*html.Node
	for n := node.FirstChild; n != nil; n = n.NextSibling {
		if n.Type == html.ElementNode {
			kids = append(kids, n)
		}
	}
	return kids
}

func attribute(node *html.Node, name string) (string, bool) {
	for _, a := range node.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val, true
		}
	}
	return "", false
}

// findElement returns the first element with the given tag anywhere in the
// tree.
func findElement(root *html.Node, want atom.Atom) *html.Node {
	var found *html.Node
	walk(root, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.DataAtom == want {
			found = n
			return false
		}
		return true
	})
	return found
}

// walk visits nodes depth-first until fn returns false.
func walk(node *html.Node, fn func(*html.Node) bool) bool {
	if node == nil {
		return true
	}
	if !fn(node) {
		return false
	}
	for n := node.FirstChild; n != nil; n = n.NextSibling {
		if !walk(n, fn) {
			return false
		}
	}
	return true
}

// textOf concatenates every descendant text node, as textContent does.
func textOf(node *html.Node) string {
	if node == nil {
		return ""
	}
	var b strings.Builder
	walk(node, func(n *html.Node) bool {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		return true
	})
	return b.String()
}

// visibleTextOf concatenates the text a reader would see, skipping the
// contents of elements that are not rendered.
//
// This does not use walk, because the two need opposite things from a false
// return: walk stops the whole traversal, whereas skipping a <script> must
// prune that subtree and carry on. Sharing the one function made every element
// containing a script yield the empty string.
func visibleTextOf(node *html.Node) string {
	var b strings.Builder
	appendVisibleText(&b, node)
	return b.String()
}

func appendVisibleText(b *strings.Builder, node *html.Node) {
	if node == nil {
		return
	}
	switch {
	case node.Type == html.TextNode:
		b.WriteString(node.Data)
		return
	case node.Type != html.ElementNode && node.Type != html.DocumentNode:
		return
	case node.Type == html.ElementNode && !rendered(node):
		return
	case node.Type == html.ElementNode && breaksLine(node):
		// Without this, a listing entry whose title and dates sit in adjacent
		// block elements reads as one run-on string.
		b.WriteByte('\n')
	}

	for n := node.FirstChild; n != nil; n = n.NextSibling {
		appendVisibleText(b, n)
	}
}

// rendered reports whether an element's text is shown to a reader.
func rendered(n *html.Node) bool {
	switch n.DataAtom {
	case atom.Script, atom.Style, atom.Noscript, atom.Template, atom.Head:
		return false
	default:
		return true
	}
}

// breaksLine reports whether an element starts a new line of visible text.
func breaksLine(n *html.Node) bool {
	switch n.DataAtom {
	case atom.P, atom.Div, atom.Br, atom.Li, atom.Tr, atom.Section, atom.Article,
		atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6,
		atom.Header, atom.Footer, atom.Ul, atom.Ol, atom.Table, atom.Dt, atom.Dd:
		return true
	default:
		return false
	}
}

func innerHTML(node *html.Node) string {
	var b strings.Builder
	for n := node.FirstChild; n != nil; n = n.NextSibling {
		if err := html.Render(&b, n); err != nil {
			return b.String()
		}
	}
	return b.String()
}

func outerHTML(node *html.Node) string {
	var b strings.Builder
	if err := html.Render(&b, node); err != nil {
		return ""
	}
	return b.String()
}

// collapse squeezes runs of whitespace to single spaces and trims, which is
// what innerText does and what almost every extracted title needs.
func collapse(s string) string {
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

// camel turns a data attribute's suffix into its dataset key: data-start-date
// becomes startDate.
func camel(s string) string {
	parts := strings.Split(s, "-")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// quoteSelectorValue renders a string as a quoted CSS attribute value.
func quoteSelectorValue(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
