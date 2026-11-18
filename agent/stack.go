package agent

import "fmt"

// Compose builds the middleware stack for a run from the built-in middleware
// (base) and the ones supplied by the user.
//
// A user middleware whose Name matches a base entry replaces it in place, so
// it keeps the built-in's position; the others are appended after base in the
// order given. Each name in exclude is then removed from the result.
//
// It returns an error for a nil entry, an empty name, a duplicate name within
// base or within user, or an exclude name that matches nothing. The inputs are
// never modified.
func Compose(base, user []Middleware, exclude []string) ([]Middleware, error) {
	if err := validateUnique("base", base); err != nil {
		return nil, err
	}
	if err := validateUnique("user", user); err != nil {
		return nil, err
	}

	stack := make([]Middleware, len(base), len(base)+len(user))
	copy(stack, base)
	for _, mw := range user {
		if i := indexByName(stack[:len(base)], mw.Name()); i >= 0 {
			stack[i] = mw
		} else {
			stack = append(stack, mw)
		}
	}
	return removeNamed(stack, exclude)
}

// validateUnique checks that every middleware is non-nil with a non-empty
// name that no other entry of the same list uses. kind labels the list in
// error messages.
func validateUnique(kind string, mws []Middleware) error {
	seen := make(map[string]bool, len(mws))
	for i, mw := range mws {
		if mw == nil {
			return fmt.Errorf("agent: %s middleware at index %d is nil", kind, i)
		}
		name := mw.Name()
		if name == "" {
			return fmt.Errorf("agent: %s middleware at index %d has an empty name", kind, i)
		}
		if seen[name] {
			return fmt.Errorf("agent: duplicate %s middleware %q", kind, name)
		}
		seen[name] = true
	}
	return nil
}

func indexByName(mws []Middleware, name string) int {
	for i, mw := range mws {
		if mw.Name() == name {
			return i
		}
	}
	return -1
}

// removeNamed returns stack without the middleware named in exclude, or an
// error if a name matches none.
func removeNamed(stack []Middleware, exclude []string) ([]Middleware, error) {
	for _, name := range exclude {
		i := indexByName(stack, name)
		if i < 0 {
			return nil, fmt.Errorf("agent: cannot exclude unknown middleware %q", name)
		}
		stack = append(stack[:i], stack[i+1:]...)
	}
	return stack, nil
}
