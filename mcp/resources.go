package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/state"
	"github.com/aswin-kevin/orchestrix/tool"
)

// listResourcesInput is the input of ToolListResources.
type listResourcesInput struct {
	Server string `json:"server,omitempty" desc:"Only list the resources of this MCP server."`
}

// readResourceInput is the input of ToolReadResource.
type readResourceInput struct {
	Server string `json:"server" desc:"The MCP server that offers the resource."`
	URI    string `json:"uri" desc:"The resource URI, as listed by list_mcp_resources."`
}

const (
	listResourcesDescription = "List the resources the connected MCP servers offer, one per line: server | uri | name | mimeType | description. A server whose listing failed gets one line saying so."
	readResourceDescription  = "Read a resource of an MCP server by its URI."
)

// listResourcesTool returns ToolListResources.
func (m *Middleware) listResourcesTool() tool.Tool {
	return tool.New(ToolListResources, listResourcesDescription,
		func(ctx context.Context, in listResourcesInput, c *tool.Call) (tool.Result, error) {
			st := callState(c)
			if in.Server != "" && !m.knownServer(in.Server, st) {
				return unknownServer(in.Server), nil
			}
			var lines []string
			for _, s := range m.servers {
				if in.Server != "" && s.cfg.Name != in.Server || !s.offersResources(st) {
					continue
				}
				resources, err := s.listResources(ctx)
				switch {
				case err != nil && in.Server != "":
					return s.failure(err), nil
				case err != nil:
					lines = append(lines, fmt.Sprintf("%s: failed to list resources: %s", s.cfg.Name, s.failure(err).Flatten()))
					continue
				}
				for _, r := range resources {
					lines = append(lines, resourceLine(s.cfg.Name, r))
				}
			}
			if len(lines) == 0 {
				return tool.Text("No resources."), nil
			}
			return tool.Text(strings.Join(lines, "\n")), nil
		},
		tool.WithReadOnly(), tool.WithConcurrent())
}

// readResourceTool returns ToolReadResource.
func (m *Middleware) readResourceTool() tool.Tool {
	return tool.New(ToolReadResource, readResourceDescription,
		func(ctx context.Context, in readResourceInput, c *tool.Call) (tool.Result, error) {
			s := m.server(in.Server)
			if s == nil || !s.offersResources(callState(c)) {
				return unknownServer(in.Server), nil
			}
			ctx, cancel := context.WithTimeout(ctx, s.requestTimeout())
			defer cancel()
			contents, err := s.client.ReadResource(ctx, in.URI)
			if err != nil {
				return s.failure(err), nil
			}
			var out tool.Result
			for _, rc := range contents {
				out.Content = append(out.Content, resourceBlock(rc, callProfile(c)))
			}
			if len(out.Content) == 0 {
				return tool.Text(fmt.Sprintf("Resource %s is empty.", in.URI)), nil
			}
			return out, nil
		},
		tool.WithReadOnly(), tool.WithConcurrent())
}

// server returns the server named name, or nil.
func (m *Middleware) server(name string) *serverConn {
	for _, s := range m.servers {
		if s.cfg.Name == name {
			return s
		}
	}
	return nil
}

// knownServer reports whether the model may learn of the server named name:
// it offers resources and is not locked in the run with State st.
func (m *Middleware) knownServer(name string, st *state.State) bool {
	s := m.server(name)
	return s != nil && s.offersResources(st)
}

// offersResources reports whether the server is connected, declared
// resources and is not locked in the run with State st.
func (s *serverConn) offersResources(st *state.State) bool {
	return s.connected() && s.client.HasResources() && !s.locked(st)
}

func (s *serverConn) listResources(ctx context.Context) ([]Resource, error) {
	ctx, cancel := context.WithTimeout(ctx, s.requestTimeout())
	defer cancel()
	return s.client.ListResources(ctx)
}

// resourceLine renders one resource for the listing; line breaks in the
// server's fields are flattened so each resource stays on one line.
func resourceLine(server string, r Resource) string {
	fields := []string{server, r.URI, r.Name, r.MimeType, r.Description}
	for i, f := range fields {
		fields[i] = strings.Join(strings.Fields(f), " ")
	}
	return strings.Join(fields, " | ")
}

// unknownServer is the error result for a server the model may not use. A
// locked server gets the same answer as a missing one.
func unknownServer(name string) tool.Result {
	return tool.Errorf("unknown MCP server %q", name)
}

// callState returns c's State, nil without a Call.
func callState(c *tool.Call) *state.State {
	if c == nil {
		return nil
	}
	return c.State
}

// callProfile returns c's model profile, the zero profile without a Call.
func callProfile(c *tool.Call) llm.Profile {
	if c == nil {
		return llm.Profile{}
	}
	return c.Profile
}
