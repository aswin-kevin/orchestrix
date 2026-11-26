package tool

// As returns the first implementer of T found in t's chain of wrappers: t
// itself, then what t's Unwrap method returns, and so on, stopping at a
// tool without an Unwrap() Tool method. ok is false when nothing in the
// chain implements T, including for a nil t.
//
// The outermost implementer wins. A wrapper that must change or hide a
// capability of the tool it wraps implements that capability itself, or
// does not implement Unwrap. A wrapper that intercepts Call of a Dispatcher
// must implement Dispatcher itself, because the agent runs the dispatch
// target instead of calling the dispatcher.
func As[T any](t Tool) (T, bool) {
	for t != nil {
		if v, ok := t.(T); ok {
			return v, true
		}
		u, ok := t.(interface{ Unwrap() Tool })
		if !ok {
			break
		}
		t = u.Unwrap()
	}
	var zero T
	return zero, false
}
