// Package termsafe escapes untrusted text (model output, tool results, error
// messages) before it is printed to a terminal, so it cannot inject control
// sequences or disguise what is shown around it.
package termsafe
