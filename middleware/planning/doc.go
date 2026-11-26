// Package planning gives the agent a todo list it maintains itself. The
// write_todos tool replaces the list, the list lives in private run state, and
// when the agent stops touching a still-open list for a while the middleware
// nudges it with a reminder that is added to the model request only.
package planning
