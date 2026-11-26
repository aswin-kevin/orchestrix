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
	"github.com/aswin-kevin/orchestrix/permission"
)

// Store defaults and limits.
const (
	// DefaultDir is the store directory when FileStoreConfig.Dir is empty. It
	// lies in the backend's internal directory, which a host typically keeps
	// out of version control.
	DefaultDir = backend.InternalDir + "/memories"
	// IndexFile is the name of the generated index inside the store
	// directory.
	IndexFile = "MEMORY.md"
	// DefaultStaleAfter is how old an entry must be before recall_memory
	// flags it as possibly outdated.
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

// Store is the structured long-term memory behind recall_memory and
// record_memory. b is the run's backend: the file store keeps entries there;
// other stores may ignore it. A Store must be safe for concurrent use.
//
// The middleware validates an entry before Record (see RecordInput) and
// renders the prompt index and recall results itself. Permission path rules
// reach a Store's tools only when it also implements Pather; FileStore does.
// Further capabilities are added as optional interfaces like Pather, so the
// interface itself stays as it is.
//
// An error from Record or Recall is shown to the model verbatim in the
// tool's error result, so it must not carry secrets. An error from Index is
// swallowed: the system prompt then shows no index, and the run goes on.
type Store interface {
	// Record creates or replaces the entry with e.Name's slug and returns
	// it as stored (File, Created, Updated set). Replacing an entry keeps
	// its Created time and sets Updated.
	Record(ctx context.Context, b backend.Backend, e Entry) (Entry, error)
	// Recall returns at most limit entries matching query, best first; an
	// empty query returns the most recently updated.
	Recall(ctx context.Context, b backend.Backend, query string, limit int) ([]Entry, error)
	// Index returns every entry, most recently updated first, for the
	// system prompt.
	Index(ctx context.Context, b backend.Backend) ([]Entry, error)
}

// Pather is implemented by a Store whose entries are files on the run's
// backend, so that permission path rules apply to its tools. Without it a
// Store is not path-checked: record_memory is still an edit (it needs an
// allow rule, an approval or acceptEdits) and recall_memory is read-only,
// but no path rule, deny rules included, reaches either tool.
//
// Every path it returns must be an absolute, clean backend path (as
// path.Clean leaves it), since the permission rules match those; a path that
// can be steered outside the store's directory by an entry's fields lets the
// rules check the wrong file.
type Pather interface {
	// RecallPaths returns what recall_memory reads, for permission rules,
	// typically the directory holding the entries.
	RecallPaths() []permission.PathAccess
	// RecordPaths returns what recording e writes. e is the entry as the
	// model gave it (Name, Description, Type and Body set; File, Created
	// and Updated not yet).
	RecordPaths(e Entry) []permission.PathAccess
	// EntryPath returns the backend path of an entry Recall returned;
	// recall_memory leaves out an entry whose path the run may not read.
	EntryPath(e Entry) string
}

// FileStoreConfig configures a FileStore.
type FileStoreConfig struct {
	// Dir is the backend directory holding the entries; it must be absolute.
	// "" means DefaultDir. Route it to a persistent backend to keep memories
	// across threads.
	Dir string
	// Now is the clock for the Created and Updated timestamps; nil means
	// time.Now.
	Now func() time.Time
}

// Entry is one stored memory. The frontmatter holds the yaml-tagged fields;
// the file body is Body.
type Entry struct {
	// Name is the memory's short title.
	Name string `yaml:"name"`
	// Description is one line saying what the memory is and when it is
	// relevant.
	Description string `yaml:"description"`
	// Type is the memory's category.
	Type Type `yaml:"type"`
	// Created is when the memory was first recorded.
	Created time.Time `yaml:"created"`
	// Updated is when the memory was last changed.
	Updated time.Time `yaml:"updated"`
	// Body is the memory's text, the part of the file after the frontmatter.
	Body string `yaml:"-"`
	// File is the entry's base name in the store directory, such as
	// "favorite-color.md".
	File string `yaml:"-"`
}

// nonSlug matches a run of characters a slug replaces with one hyphen.
var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug derives the file-name stem for an entry name: lowercase, every run of
// characters outside [a-z0-9] collapsed to "-", trimmed, and cut to
// MaxSlugLen. A name with no letters or digits is an error. Different names
// can share a slug ("C++ tips" and "C tips"); FileStore refuses to record one
// whose slug belongs to an entry of another name.
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

// FileStore is the default Store: one Markdown file per entry in a backend
// directory, with a frontmatter header, plus a generated MEMORY.md index
// listing every entry. It implements Pather, so permission path rules see
// the directory and the files it writes. It is safe for concurrent use.
type FileStore struct {
	// mu serializes Record, whose read-modify-write of an entry and rebuild
	// of the index must not interleave.
	mu  sync.Mutex
	dir string
	now func() time.Time
}

// NewFileStore returns a FileStore. It fails when Dir is not absolute.
func NewFileStore(cfg FileStoreConfig) (*FileStore, error) {
	s := &FileStore{dir: cfg.Dir, now: cfg.Now}
	if s.dir == "" {
		s.dir = DefaultDir
	}
	if !path.IsAbs(s.dir) {
		return nil, fmt.Errorf("middleware/memory: store directory %q must be absolute", s.dir)
	}
	s.dir = path.Clean(s.dir)
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// FileStore is a Store that reports its paths.
var (
	_ Store  = (*FileStore)(nil)
	_ Pather = (*FileStore)(nil)
)

// errNoBackend is returned by a FileStore method given a nil backend.
var errNoBackend = errors.New("no backend to keep memories in")

func (s *FileStore) entryPath(slug string) string { return path.Join(s.dir, slug+entryExt) }
func (s *FileStore) indexPath() string            { return path.Join(s.dir, IndexFile) }

// RecallPaths implements Pather: recall reads the store directory. The
// entries read are not known before the call, so the rules see the
// directory, the same way ls reports its root; each entry is then checked
// through EntryPath.
func (s *FileStore) RecallPaths() []permission.PathAccess {
	return []permission.PathAccess{{Path: s.dir, Op: permission.OpRead}}
}

// RecordPaths implements Pather: recording writes MEMORY.md and the entry's
// file, which is left out when e.Name has no slug (Record then fails).
func (s *FileStore) RecordPaths(e Entry) []permission.PathAccess {
	access := []permission.PathAccess{{Path: s.indexPath(), Op: permission.OpWrite}}
	if slug, err := Slug(e.Name); err == nil {
		access = append(access, permission.PathAccess{Path: s.entryPath(slug), Op: permission.OpWrite})
	}
	return access
}

// EntryPath implements Pather: the entry's file in the store directory. Only
// the base name of e.File counts, so an entry whose File holds a directory
// part (such as "../x.md") still maps to a path inside the directory. A File
// whose base name is "", ".", ".." or "/" maps to the directory itself, never
// to its parent.
func (s *FileStore) EntryPath(e Entry) string {
	switch base := path.Base(e.File); base {
	case ".", "..", "/":
		return s.dir
	default:
		return path.Join(s.dir, base)
	}
}

// load reads every entry in the directory, newest first. Files that cannot be
// read or parsed are returned by base name in unreadable. A missing
// directory is an empty store.
func (s *FileStore) load(ctx context.Context, b backend.Backend) (entries []Entry, unreadable []string, err error) {
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
		e, err := s.readEntry(ctx, b, file)
		if err != nil {
			unreadable = append(unreadable, file)
			continue
		}
		entries = append(entries, e)
	}
	slices.SortStableFunc(entries, func(a, b Entry) int { return b.Updated.Compare(a.Updated) })
	slices.Sort(unreadable)
	return entries, unreadable, nil
}

// errNotEntry marks a file that exists but is not a well-formed entry:
// binary, without frontmatter, with malformed YAML or with an empty name.
var errNotEntry = errors.New("not a readable memory")

// readEntry parses one file of the store. It fails with the backend's error
// (backend.ErrNotFound when the file is missing) or with errNotEntry.
func (s *FileStore) readEntry(ctx context.Context, b backend.Backend, file string) (Entry, error) {
	res, err := b.Read(ctx, path.Join(s.dir, file), 0, 0)
	if err != nil {
		return Entry{}, err
	}
	if res.Binary {
		return Entry{}, errNotEntry
	}
	var e Entry
	body, ok, err := frontmatter.Parse([]byte(strings.Join(res.Lines, "\n")), &e)
	if err != nil || !ok || e.Name == "" {
		return Entry{}, errNotEntry
	}
	e.Body, e.File = strings.TrimSpace(body), file
	return e, nil
}

// validate checks an entry to record and returns its slug. Its messages are
// for the model, so they carry no package prefix.
func validate(e Entry) (string, error) {
	switch {
	case strings.TrimSpace(e.Name) == "" || strings.TrimSpace(e.Description) == "" || strings.TrimSpace(e.Body) == "":
		return "", errors.New("name, description and content must not be empty")
	case strings.ContainsAny(e.Name+e.Description, "\r\n"):
		return "", errors.New("name and description must be a single line")
	case len([]rune(e.Name)) > MaxNameChars:
		return "", fmt.Errorf("name is over %d characters", MaxNameChars)
	case len([]rune(e.Description)) > MaxDescriptionChars:
		return "", fmt.Errorf("description is over %d characters", MaxDescriptionChars)
	case len(e.Body) > MaxEntryBytes:
		return "", fmt.Errorf("content is over %d bytes", MaxEntryBytes)
	case !slices.Contains(types, e.Type):
		return "", fmt.Errorf("type %q is not one of user, feedback, project, reference", e.Type)
	}
	slug, err := Slug(e.Name)
	if err == nil && slug == reservedSlug {
		return "", fmt.Errorf("name %q is reserved; pick a more specific one", e.Name)
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

// unreadableError reports that a name's slug is the file of something that is
// not a readable memory, which record refuses to overwrite.
type unreadableError struct{ file string }

func (e *unreadableError) Error() string {
	return fmt.Sprintf("%s exists but is not a readable memory", e.file)
}

// sameName reports whether two entry names are the same up to case and
// whitespace.
func sameName(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " "))
}

// Record implements Store: it validates e, writes its file and rebuilds
// MEMORY.md. Name, Description and Body are stored trimmed. An existing
// entry of the same name (ignoring case and spacing) keeps its Created time.
// An existing entry of another name that shares the slug is left alone and
// an error returned; so is a file at the slug's path that is not a readable
// entry. Only a missing file means create.
func (s *FileStore) Record(ctx context.Context, b backend.Backend, e Entry) (Entry, error) {
	stored, _, err := s.record(ctx, b, e)
	return stored, err
}

// record is Record, also reporting whether it replaced an existing entry.
// A refused collision is a *collisionError, an unreadable file at the slug's
// path an *unreadableError.
func (s *FileStore) record(ctx context.Context, b backend.Backend, in Entry) (e Entry, updated bool, err error) {
	slug, err := validate(in)
	if err != nil {
		return Entry{}, false, err
	}
	if b == nil {
		return Entry{}, false, errNoBackend
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	e = Entry{
		Name: strings.TrimSpace(in.Name), Description: strings.TrimSpace(in.Description),
		Type: in.Type, Created: now, Updated: now,
	}
	old, err := s.readEntry(ctx, b, slug+entryExt)
	switch {
	case err == nil:
		if !sameName(old.Name, e.Name) {
			return Entry{}, false, &collisionError{name: e.Name, existing: old.Name, file: old.File}
		}
		e.Created, updated = old.Created, true
	case errors.Is(err, errNotEntry):
		return Entry{}, false, &unreadableError{file: s.entryPath(slug)}
	case !errors.Is(err, backend.ErrNotFound):
		return Entry{}, false, err
	}
	body := strings.TrimSpace(in.Body)
	data, err := frontmatter.Render(e, body+"\n")
	if err != nil {
		return Entry{}, false, err
	}
	if err := b.Write(ctx, s.entryPath(slug), data); err != nil {
		return Entry{}, false, err
	}
	e.Body, e.File = body, slug+entryExt
	return e, updated, s.rebuildIndex(ctx, b)
}

// rebuildIndex regenerates MEMORY.md from the directory, which is the ground
// truth: entries sorted by name, then unreadable files.
func (s *FileStore) rebuildIndex(ctx context.Context, b backend.Backend) error {
	entries, unreadable, err := s.load(ctx, b)
	if err != nil {
		return err
	}
	return b.Write(ctx, s.indexPath(), []byte(renderIndex(entries, unreadable)))
}

// renderIndex renders the index FileStore writes to MEMORY.md and the
// middleware puts in the system prompt: a heading, the entries sorted by
// name, then the unreadable files.
func renderIndex(entries []Entry, unreadable []string) string {
	entries = slices.Clone(entries)
	slices.SortStableFunc(entries, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	var out strings.Builder
	out.WriteString(indexHeading)
	for _, e := range entries {
		if e.File == "" {
			fmt.Fprintf(&out, "- %s (%s) — %s\n", e.Name, e.Type, e.Description)
			continue
		}
		fmt.Fprintf(&out, "- [%s](%s) (%s) — %s\n", e.Name, e.File, e.Type, e.Description)
	}
	for _, f := range unreadable {
		fmt.Fprintf(&out, "- %s (unreadable)\n", f)
	}
	return out.String()
}

// Index implements Store: every readable entry, most recently updated
// first. Files that are not readable entries are left out (MEMORY.md lists
// them). A missing directory is an empty store.
func (s *FileStore) Index(ctx context.Context, b backend.Backend) ([]Entry, error) {
	if b == nil {
		return nil, errNoBackend
	}
	entries, _, err := s.load(ctx, b)
	return entries, err
}

// Recall implements Store: entries scored by keyword matches in the name,
// description and body, best first; ties keep the newest first. The query is
// split at every character that is not a letter or digit, and words shorter
// than two characters are ignored, so a query of only such words matches
// nothing. limit <= 0 means DefaultRecallLimit and is capped at
// MaxRecallLimit.
func (s *FileStore) Recall(ctx context.Context, b backend.Backend, query string, limit int) ([]Entry, error) {
	return s.recall(ctx, b, query, limit, func(string) bool { return true })
}

// recall is Recall with keep, which filters out the entries, by path, that
// the caller may not see before the limit applies.
func (s *FileStore) recall(ctx context.Context, b backend.Backend, query string, limit int, keep func(path string) bool) ([]Entry, error) {
	if b == nil {
		return nil, errNoBackend
	}
	entries, _, err := s.load(ctx, b)
	if err != nil {
		return nil, err
	}
	entries = slices.DeleteFunc(entries, func(e Entry) bool { return !keep(s.EntryPath(e)) })
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

// format renders a recalled entry for the model, with a note when it is
// older than staleAfter at now.
func format(e Entry, now time.Time, staleAfter time.Duration) string {
	out := fmt.Sprintf("## %s (%s, updated %s)\n%s\n\n%s", e.Name, e.Type, e.Updated.Format(time.DateOnly), e.Description, e.Body)
	if age := now.Sub(e.Updated); age > staleAfter {
		out += fmt.Sprintf("\n(Note: last updated %d days ago; it may be outdated. Verify before relying on it.)", int(age/hoursPerDay))
	}
	return out
}
