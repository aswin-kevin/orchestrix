package filesystem

import (
	"context"
	"errors"

	"github.com/aswin-kevin/orchestrix/backend"
	"github.com/aswin-kevin/orchestrix/permission"
	"github.com/aswin-kevin/orchestrix/tool"
)

// target is a file a mutating tool is about to change: the backend and the
// cleaned path.
type target struct {
	backend backend.Backend
	path    string
}

// resolveTarget prepares a mutating call. A non-nil result is a failure to
// return to the model as is.
func (m *middleware) resolveTarget(ctx context.Context, c *tool.Call, rawPath string) (target, *tool.Result) {
	fail := func(r tool.Result) (target, *tool.Result) { return target{}, &r }
	b, err := m.backendFor(c)
	if err != nil {
		return fail(tool.Errorf("%v", err))
	}
	path, err := backend.Clean(rawPath)
	if err != nil {
		return fail(errorResult(err, rawPath))
	}
	fresh, err := checkFresh(ctx, b, c, path)
	if err != nil {
		return fail(errorResult(err, path))
	}
	if !fresh {
		return fail(tool.Errorf("%s", staleReadMessage))
	}
	return target{backend: b, path: path}, nil
}

type writeFileInput struct {
	Path    string `json:"path" desc:"File to write, absolute from the workspace root. Parent directories are created."`
	Content string `json:"content" desc:"Complete new content of the file."`
}

func (m *middleware) writeFileTool() tool.Tool {
	return tool.New(ToolWriteFile,
		m.description(ToolWriteFile, "Create a file or replace its entire content. An existing file must have been read first (and not changed since); creating a new file needs no read. Prefer edit_file for changing part of a file."),
		m.writeFile,
		tool.WithEdit(),
		tool.WithPaths(accessFor(permission.OpWrite, func(in writeFileInput) string { return in.Path })),
	)
}

func (m *middleware) writeFile(ctx context.Context, in writeFileInput, c *tool.Call) (tool.Result, error) {
	t, failure := m.resolveTarget(ctx, c, in.Path)
	if failure != nil {
		return *failure, nil
	}
	if err := t.backend.Write(ctx, t.path, []byte(in.Content)); err != nil {
		return errorResult(err, t.path), nil
	}
	if err := recordCurrent(ctx, t.backend, c, t.path); err != nil {
		return errorResult(err, t.path), nil
	}
	return textf("Wrote %d bytes to %s", len(in.Content), t.path), nil
}

type editFileInput struct {
	Path       string `json:"path" desc:"File to edit, absolute from the workspace root."`
	OldString  string `json:"old_string" desc:"Exact text to replace, including whitespace. Must be unique in the file unless replace_all is set."`
	NewString  string `json:"new_string" desc:"Replacement text. Must differ from old_string."`
	ReplaceAll bool   `json:"replace_all,omitempty" desc:"Replace every occurrence instead of requiring a unique match."`
}

func (m *middleware) editFileTool() tool.Tool {
	return tool.New(ToolEditFile,
		m.description(ToolEditFile, "Replace exact text in a file. old_string must match exactly once (add surrounding context to make it unique) unless replace_all is set. The file must have been read first and not changed since."),
		m.editFile,
		tool.WithEdit(),
		tool.WithPaths(accessFor(permission.OpWrite, func(in editFileInput) string { return in.Path })),
	)
}

func (m *middleware) editFile(ctx context.Context, in editFileInput, c *tool.Call) (tool.Result, error) {
	t, failure := m.resolveTarget(ctx, c, in.Path)
	if failure != nil {
		return *failure, nil
	}
	res, err := t.backend.Edit(ctx, t.path, in.OldString, in.NewString, in.ReplaceAll)
	if err != nil {
		return errorResult(err, t.path), nil
	}
	if err := recordCurrent(ctx, t.backend, c, t.path); err != nil {
		return errorResult(err, t.path), nil
	}
	return textf("Replaced %d occurrence(s) in %s", res.Occurrences, t.path), nil
}

type editStep struct {
	OldString  string `json:"old_string" desc:"Exact text to replace."`
	NewString  string `json:"new_string" desc:"Replacement text."`
	ReplaceAll bool   `json:"replace_all,omitempty" desc:"Replace every occurrence instead of requiring a unique match."`
}

type multiEditInput struct {
	Path  string     `json:"path" desc:"File to edit, absolute from the workspace root."`
	Edits []editStep `json:"edits" desc:"Edits applied in order; each sees the result of the previous ones."`
}

func (m *middleware) multiEditTool() tool.Tool {
	return tool.New(ToolMultiEdit,
		m.description(ToolMultiEdit, "Apply several edits to one file in order, all or nothing: if any edit fails, the file is left unchanged. Same matching rules and read-first requirement as edit_file."),
		m.multiEdit,
		tool.WithEdit(),
		tool.WithPaths(accessFor(permission.OpWrite, func(in multiEditInput) string { return in.Path })),
	)
}

func (m *middleware) multiEdit(ctx context.Context, in multiEditInput, c *tool.Call) (tool.Result, error) {
	if len(in.Edits) == 0 {
		return tool.Errorf("edits is empty"), nil
	}
	t, failure := m.resolveTarget(ctx, c, in.Path)
	if failure != nil {
		return *failure, nil
	}
	editor, ok := t.backend.(backend.MultiEditor)
	if !ok {
		return tool.Errorf("this backend does not support atomic multi_edit; use edit_file"), nil
	}
	ops := make([]backend.EditOp, len(in.Edits))
	for i, e := range in.Edits {
		ops[i] = backend.EditOp{Old: e.OldString, New: e.NewString, ReplaceAll: e.ReplaceAll}
	}
	if _, err := editor.MultiEdit(ctx, t.path, ops); err != nil {
		return multiEditFailure(err, t.path), nil
	}
	if err := recordCurrent(ctx, t.backend, c, t.path); err != nil {
		return errorResult(err, t.path), nil
	}
	return textf("Applied %d edit(s) to %s", len(in.Edits), t.path), nil
}

// multiEditFailure names the failing edit when the backend says which it was.
func multiEditFailure(err error, path string) tool.Result {
	var step *backend.EditStepError
	if !errors.As(err, &step) {
		return errorResult(err, path)
	}
	return stepError(step.Index, step.Total, errorResult(step.Err, path))
}

type deleteInput struct {
	Path string `json:"path" desc:"File or directory to delete, absolute from the workspace root."`
}

func (m *middleware) deleteTool() tool.Tool {
	return tool.New(ToolDelete,
		m.description(ToolDelete, "Delete a file or directory."),
		m.deleteFile,
		tool.WithEdit(),
		tool.WithPaths(accessFor(permission.OpWrite, func(in deleteInput) string { return in.Path })),
	)
}

func (m *middleware) deleteFile(ctx context.Context, in deleteInput, c *tool.Call) (tool.Result, error) {
	b, err := m.backendFor(c)
	if err != nil {
		return tool.Errorf("%v", err), nil
	}
	path, err := backend.Clean(in.Path)
	if err != nil {
		return errorResult(err, in.Path), nil
	}
	if err := b.Delete(ctx, path); err != nil {
		return errorResult(err, path), nil
	}
	forgetUnder(c, path)
	return textf("Deleted %s", path), nil
}
