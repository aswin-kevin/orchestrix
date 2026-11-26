// Package evals runs scenarios against a live model several times each and
// measures how often they pass. It lives in its own module so the root
// module's tests stay offline.
//
// A trial ends in one of four ways. It passes; it fails because the model did
// not do what the scenario needs (Failure); it breaks a harness invariant
// (Violation), which is a bug in the harness whatever the model did; or it
// cannot be judged because the model call itself failed (Error). Pass rates
// vary with the model, so they are compared against a stored baseline with a
// tolerance. Violations are never tolerated.
package evals
