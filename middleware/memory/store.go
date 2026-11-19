package memory

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/internal/frontmatter"
)

// Store defaults and limits.
const (
	// DefaultDir is the store directory when StoreConfig.Dir is empty. It
	// lies in the backend's internal directory, which a host typically keeps
	// out of version control.
	DefaultDir = backend.InternalDir + "/memories"
	// IndexFile is the name of the generated index inside the store
	// directory.
	IndexFile = "MEMORY.md"
	// DefaultStaleAfter is how old an entry must be before recall flags it as
	// possibly outdated.
	DefaultStaleAfter = 30 * 24 * time.Hour
	// DefaultRecallLimit is how many memories recall_memory returns when the
	// call sets no limit.
	DefaultRecallLimit = 5
	// MaxRecallLimit caps the limit a call may request.
	MaxRecallLimit = 20
	// MaxIndexBytes caps the index text placed in the system prompt.
	MaxIndexBytes = 16 << 10
	// MaxEntryBytes caps the content of one entry.
	MaxEntryBytes = 16 << 10
	// MaxDescriptionChars caps an entry's description, in runes.
	MaxDescriptionChars = 300
	// MaxNameChars caps an entry's name, in runes. It equals MaxSlugLen, so
	// an ASCII name is never cut to build its file name.
	MaxNameChars = MaxSlugLen
	// MaxRecallBytes caps the text one recall_memory call returns.
	MaxRecallBytes = 32 << 10
	// MaxSlugLen caps the file-name stem derived from an entry's name.
	MaxSlugLen = 64

	// ToolRecall and ToolRecord are the names of the store's tools.
	ToolRecall = "recall_memory"
	ToolRecord = "record_memory"

	entryExt = ".md"
	// reservedSlug would collide with IndexFile on a case-insensitive disk.
	reservedSlug = "memory"
	indexHeading = "# Memory index\n\n"
	hoursPerDay  = 24 * time.Hour
	// minTermLen is the shortest query word that counts as a keyword.
	minTermLen = 2
	// Recall scoring: weights of a match in the name and description, and of
	// each body occurrence up to maxBodyHits.
	nameWeight, descriptionWeight, maxBodyHits = 3, 2, 3
)

// Type classifies a memory entry.
type Type string

// Entry types.
const (
	TypeUser      Type = "user"
	TypeFeedback  Type = "feedback"
	TypeProject   Type = "project"
	TypeReference Type = "reference"
)

var types = []Type{TypeUser, TypeFeedback, TypeProject, TypeReference}

// StoreConfig configures the structured long-term memory store: one Markdown
// file per entry, with a frontmatter header, plus a generated MEMORY.md index.
type StoreConfig struct {
	// Dir is the backend directory holding the entries; it must be absolute.
	// "" means DefaultDir. Route it to a persistent backend to keep memories
	// across threads.
	Dir string
	// StaleAfter is the age after which recall adds an "outdated" note; 0
	// means DefaultStaleAfter and a negative value is rejected by New.
	StaleAfter time.Duration
	// Now is the clock for timestamps and staleness; nil means time.Now.
	Now func() time.Time
	// DisableIndexInPrompt keeps the index out of the system prompt. The
	// tools stay available.
	DisableIndexInPrompt bool
}

// Entry is one stored memory. The frontmatter holds the yaml-tagged fields;
// the file body is Body.
type Entry struct {
	Name        string    `yaml:"name"`
	Description string    `yaml:"description"`
	Type        Type      `yaml:"type"`
	Created     time.Time `yaml:"created"`
	Updated     time.Time `yaml:"updated"`
	Body        string    `yaml:"-"`
	// File is the entry's base name in the store directory, such as
	// "favorite-color.md".
	File string `yaml:"-"`
}

// nonSlug matches a run of characters a slug replaces with one hyphen.
var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug derives the file-name stem for an entry name: lowercase, every run of
// characters outside [a-z0-9] collapsed to "-", trimmed, and cut to
// MaxSlugLen. A name with no letters or digits is an error. Different names
// can share a slug ("C++ tips" and "C tips"); recording one whose slug
// belongs to an entry of another name is refused (see record).
func Slug(name string) (string, error) {
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(slug) > MaxSlugLen {
		slug = strings.Trim(slug[:MaxSlugLen], "-")
	}
	if slug == "" {
		return "", fmt.Errorf("name %q has no letters or digits to build a file name from", name)
	}
	return slug, nil
}

// store is the resolved StoreConfig; only mu changes. mu serializes record,
// whose read-modify-write of an entry and rebuild of the index must not
// interleave.
type store struct {
	mu         sync.Mutex
	dir        string
	staleAfter time.Duration
	now        func() time.Time
	showIndex  bool
}

func newStore(cfg StoreConfig) (*store, error) {
	if cfg.StaleAfter < 0 {
		return nil, fmt.Errorf("memory: StaleAfter must not be negative, got %v", cfg.StaleAfter)
	}
	s := &store{dir: cfg.Dir, staleAfter: cfg.StaleAfter, now: cfg.Now, showIndex: !cfg.DisableIndexInPrompt}
	if s.dir == "" {
		s.dir = DefaultDir
	}
	if !path.IsAbs(s.dir) {
		return nil, fmt.Errorf("memory: store directory %q must be absolute", s.dir)
	}
	s.dir = path.Clean(s.dir)
	if s.staleAfter == 0 {
		s.staleAfter = DefaultStaleAfter
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

func (s *store) entryPath(slug string) string { return path.Join(s.dir, slug+entryExt) }
func (s *store) indexPath() string            { return path.Join(s.dir, IndexFile) }

// load reads every entry in the directory, newest first. Files that cannot be
// read or parsed are returned by base name in unreadable. A missing
// directory is an empty store.
func (s *store) load(ctx context.Context, b backend.Backend) (entries []Entry, unreadable []string, err error) {
	infos, err := b.Ls(ctx, s.dir)
	if errors.Is(err, backend.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, info := range infos {
		file := path.Base(info.Path)
		if info.IsDir || file == IndexFile || path.Ext(file) != entryExt {
			continue
		}
		e, ok := s.readEntry(ctx, b, file)
		if !ok {
			unreadable = append(unreadable, file)
			continue
		}
		entries = append(entries, e)
	}
	slices.SortStableFunc(entries, func(a, b Entry) int { return b.Updated.Compare(a.Updated) })
	slices.Sort(unreadable)
	return entries, unreadable, nil
}

// readEntry parses one file of the store; ok is false when it is not a
// well-formed entry.
func (s *store) readEntry(ctx context.Context, b backend.Backend, file string) (Entry, bool) {
	res, err := b.Read(ctx, path.Join(s.dir, file), 0, 0)
	if err != nil || res.Binary {
		return Entry{}, false
	}
	var e Entry
	body, ok, err := frontmatter.Parse([]byte(strings.Join(res.Lines, "\n")), &e)
	if err != nil || !ok || e.Name == "" {
		return Entry{}, false
	}
	e.Body, e.File = strings.TrimSpace(body), file
	return e, true
}

// validate checks a record request and returns the entry's slug.
func (in RecordInput) validate() (string, error) {
	switch {
	case strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.Description) == "" || strings.TrimSpace(in.Content) == "":
		return "", errors.New("name, description and content must not be empty")
	case strings.ContainsAny(in.Name+in.Description, "\r\n"):
		return "", errors.New("name and description must be a single line")
	case len([]rune(in.Name)) > MaxNameChars:
		return "", fmt.Errorf("name is over %d characters", MaxNameChars)
	case len([]rune(in.Description)) > MaxDescriptionChars:
		return "", fmt.Errorf("description is over %d characters", MaxDescriptionChars)
	case len(in.Content) > MaxEntryBytes:
		return "", fmt.Errorf("content is over %d bytes", MaxEntryBytes)
	case !slices.Contains(types, in.Type):
		return "", fmt.Errorf("type %q is not one of user, feedback, project, reference", in.Type)
	}
	slug, err := Slug(in.Name)
	if err == nil && slug == reservedSlug {
		return "", fmt.Errorf("name %q is reserved; pick a more specific one", in.Name)
	}
	return slug, err
}

// collisionError reports that a name's slug is the file of an entry with
// another name, which record refuses to overwrite.
type collisionError struct {
	name, existing, file string
}

func (e *collisionError) Error() string {
	return fmt.Sprintf("memory %q would be stored in %s, which holds the memory %q", e.name, e.file, e.existing)
}

// sameName reports whether two entry names are the same up to case and
// whitespace.
func sameName(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " "))
}

// record creates or updates the entry for in and rebuilds the index. An
// existing entry of the same name (see sameName) keeps its Created time; an
// existing entry of another name that shares the slug is left alone and a
// *collisionError returned. It returns the entry's path and whether it
// replaced an existing entry.
func (s *store) record(ctx context.Context, b backend.Backend, in RecordInput) (p string, updated bool, err error) {
	slug, err := in.validate()
	if err != nil {
		return "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	p = s.entryPath(slug)
	e := Entry{
		Name: strings.TrimSpace(in.Name), Description: strings.TrimSpace(in.Description),
		Type: in.Type, Created: now, Updated: now,
	}
	if old, ok := s.readEntry(ctx, b, slug+entryExt); ok {
		if !sameName(old.Name, e.Name) {
			return "", false, &collisionError{name: e.Name, existing: old.Name, file: old.File}
		}
		e.Created, updated = old.Created, true
	}
	data, err := frontmatter.Render(e, strings.TrimSpace(in.Content)+"\n")
	if err != nil {
		return "", false, err
	}
	if err := b.Write(ctx, p, data); err != nil {
		return "", false, err
	}
	return p, updated, s.rebuildIndex(ctx, b)
}

// rebuildIndex regenerates MEMORY.md from the directory, which is the ground
// truth: entries sorted by name, then unreadable files.
func (s *store) rebuildIndex(ctx context.Context, b backend.Backend) error {
	entries, unreadable, err := s.load(ctx, b)
	if err != nil {
		return err
	}
	slices.SortStableFunc(entries, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	var out strings.Builder
	out.WriteString(indexHeading)
	for _, e := range entries {
		fmt.Fprintf(&out, "- [%s](%s) (%s) — %s\n", e.Name, e.File, e.Type, e.Description)
	}
	for _, f := range unreadable {
		fmt.Fprintf(&out, "- %s (unreadable)\n", f)
	}
	return b.Write(ctx, s.indexPath(), []byte(out.String()))
}

// recall returns the entries matching query, best first. An empty query
// returns the newest. keep filters out entries the caller may not see.
func (s *store) recall(ctx context.Context, b backend.Backend, query string, limit int, keep func(path string) bool) ([]Entry, error) {
	entries, _, err := s.load(ctx, b)
	if err != nil {
		return nil, err
	}
	entries = slices.DeleteFunc(entries, func(e Entry) bool { return !keep(path.Join(s.dir, e.File)) })
	if strings.TrimSpace(query) != "" {
		terms := queryTerms(query)
		if len(terms) == 0 {
			return nil, nil // only one-letter words: matches nothing
		}
		scores := make(map[string]int, len(entries))
		for _, e := range entries {
			scores[e.File] = score(e, terms)
		}
		entries = slices.DeleteFunc(entries, func(e Entry) bool { return scores[e.File] == 0 })
		slices.SortStableFunc(entries, func(a, b Entry) int { return scores[b.File] - scores[a.File] })
	}
	return entries[:min(len(entries), clampLimit(limit))], nil
}

func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultRecallLimit
	case limit > MaxRecallLimit:
		return MaxRecallLimit
	}
	return limit
}

// queryTerms splits a query into lowercased keywords.
func queryTerms(query string) []string {
	words := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return slices.DeleteFunc(words, func(w string) bool { return len([]rune(w)) < minTermLen })
}

// score rates how well e matches the terms; 0 means no match.
func score(e Entry, terms []string) int {
	name, desc, body := strings.ToLower(e.Name), strings.ToLower(e.Description), strings.ToLower(e.Body)
	total := 0
	for _, t := range terms {
		if strings.Contains(name, t) {
			total += nameWeight
		}
		if strings.Contains(desc, t) {
			total += descriptionWeight
		}
		total += min(strings.Count(body, t), maxBodyHits)
	}
	return total
}

// format renders a recalled entry for the model.
func (s *store) format(e Entry) string {
	out := fmt.Sprintf("## %s (%s, updated %s)\n%s\n\n%s", e.Name, e.Type, e.Updated.Format(time.DateOnly), e.Description, e.Body)
	if age := s.now().Sub(e.Updated); age > s.staleAfter {
		out += fmt.Sprintf("\n(Note: last updated %d days ago; it may be outdated. Verify before relying on it.)", int(age/hoursPerDay))
	}
	return out
}
