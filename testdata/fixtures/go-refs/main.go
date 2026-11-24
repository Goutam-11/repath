package main

// Engine and Holder exercise a one-hop chained receiver: h.engine.start()
// is typed from the declared field, not from the method name.
type Engine struct{ protocol string }

func (e *Engine) start() {}

type Holder struct{ engine *Engine }

// Registry exercises a function value in a keyed composite-literal field.
type Registry struct{ handler func() }

func register(f func()) {}

func newEngine() *Engine { return &Engine{} }

func newHolder() *Holder { return &Holder{engine: newEngine()} }

func buildRegistry() Registry {
	return Registry{handler: handler}
}

func main() {
	h := newHolder()
	run(h)
}

func run(h *Holder) {
	// handler is a function value in argument position: a REFERENCES edge.
	register(handler)
	// cleanup is called inside a deferred function literal; the reference
	// belongs to run (Go has no closure symbols).
	defer func() { cleanup() }()
	// h.engine.start(): chained selector, one hop.
	h.engine.start()
	// newEngine().start(): the callee is a call expression, so the inner call
	// must still be walked.
	newEngine().start()
}

func prepare() {}
