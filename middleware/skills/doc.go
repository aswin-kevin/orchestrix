// Package skills loads SKILL.md directories and offers them to the model: a
// catalog of names and descriptions in the system prompt, plus the full body
// of pinned skills.
//
// The model loads a skill with the skill tool, which returns the skill's
// instructions framed by Marker and closing tag, and can read the skill's
// other files through the tool's file parameter. Loading a skill records it
// in the run (see Loaded) and unlocks (deferred.Unlock) the tools its
// allowed-tools names and the tools of its MCP servers
// (names.MCPPrefix(server) + "*").
// Config.Tools are offered gated (deferred.Gate), so they stay refused until
// a loaded skill unlocks them; add the deferred middleware to also keep their
// schemas out of the request until then.
//
// Loaded instructions stay in effect after compaction: every request whose
// messages no longer show a loaded skill's full tool result gets those
// instructions back under StandingHeader at the start of the conversation.
// This only edits the request view, never State. The summarize middleware
// rebuilds the view from State, so put this middleware after it.
//
// # Trust
//
// A skill's name, description and body come from files and are written into
// the system prompt, so whoever can write a skills directory can steer the
// agent. Only load sources you trust. The framing is protected: descriptions
// are reduced to one line, and a literal frame tag inside any skill text is
// neutralized so it cannot end its element.
package skills
