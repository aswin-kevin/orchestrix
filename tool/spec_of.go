package tool

import "maps"

// SpecOf returns a Spec that builds a tool behaving like t, so a tool can be
// rebuilt with a change, such as a new Name. For a tool made by Build it is
// a copy of the Spec it was built from. For any other tool it takes Name,
// Description, Schema and Call from t and every optional func from the
// capability interfaces As finds in t; ShellSubject and RawInput are read
// once, here. Schema is a shallow copy: setting or deleting one of its
// top-level keys leaves t unchanged, but maps nested in it (such as
// "properties") are still shared with t, so copy one before changing it.
// Build(SpecOf(t)) never panics for a tool with a valid name.
//
// Markers defined outside this package, such as those of the deferred
// middleware, are not part of a Spec and are lost.
func SpecOf(t Tool) Spec {
	switch b := t.(type) {
	case *built:
		return b.specCopy()
	case *builtDispatcher:
		return b.specCopy()
	}
	s := Spec{
		Name:        t.Name(),
		Description: t.Description(),
		Schema:      maps.Clone(t.Schema()),
		Run:         t.Call,
	}
	if v, ok := As[ReadOnlyer](t); ok {
		s.ReadOnly = v.ReadOnly
	}
	if v, ok := As[ConcurrencySafer](t); ok {
		s.Concurrent = v.ConcurrencySafe
	}
	if v, ok := As[Editor](t); ok {
		s.Edit = v.IsEdit
	}
	if v, ok := As[Subjecter](t); ok {
		s.Subject = v.Subject
	}
	if v, ok := As[ShellSubjecter](t); ok {
		s.ShellSubject = v.ShellSubject()
	}
	if v, ok := As[PathAccessor](t); ok {
		s.Paths = v.Paths
	}
	if v, ok := As[PermissionChecker](t); ok {
		s.Permission = v.CheckPermission
	}
	if v, ok := As[RawInputer](t); ok {
		s.RawInput = v.AcceptsRawInput()
	}
	if v, ok := As[Dispatcher](t); ok {
		s.Dispatch = v.Dispatch
	}
	if v, ok := As[Availability](t); ok {
		s.Available = v.Available
	}
	return s
}

// specCopy returns b's Spec with its own copy of the Schema map.
func (b *built) specCopy() Spec {
	s := b.spec
	s.Schema = maps.Clone(s.Schema)
	return s
}
