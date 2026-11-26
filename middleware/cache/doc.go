// Package cache asks the model provider to cache the recent conversation, so
// each turn only pays for the messages added since the previous one. The
// system prompt boundary is owned by whoever builds the request; this
// middleware only controls the trailing-message breakpoints.
package cache
