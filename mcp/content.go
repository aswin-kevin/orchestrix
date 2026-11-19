package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/tool"
)

// toolResult maps a tool call's result to the agent's: one block per content
// block; when there is none, the structured content as compact JSON.
func toolResult(res *CallToolResult) tool.Result {
	var out tool.Result
	for _, c := range res.Content {
		out.Content = append(out.Content, contentBlock(c))
	}
	if len(out.Content) == 0 && len(res.StructuredContent) > 0 {
		out.Content = []llm.Block{llm.TextBlock(compactJSON(res.StructuredContent))}
	}
	out.IsError = res.IsError
	return out
}

// contentBlock maps one content block. Images become image blocks; audio
// and unknown kinds become a note, since the model cannot take them.
func contentBlock(c Content) llm.Block {
	switch c.Type {
	case "text":
		return llm.TextBlock(c.Text)
	case "image":
		return imageBlock(c.MimeType, c.Data)
	case "audio":
		return llm.TextBlock(fmt.Sprintf("[audio content omitted: %s]", c.MimeType))
	case "resource":
		if c.Resource == nil {
			return llm.TextBlock("[empty resource omitted]")
		}
		return resourceBlock(*c.Resource)
	case "resource_link":
		return llm.TextBlock(fmt.Sprintf("Resource link: %s (%s)", c.URI, c.Name))
	default:
		return llm.TextBlock(fmt.Sprintf("[%q content omitted]", c.Type))
	}
}

// resourceBlock maps the content of a resource: its text, an image blob as
// an image, any other blob as a note.
func resourceBlock(r ResourceContent) llm.Block {
	switch {
	case r.Blob != "" && strings.HasPrefix(r.MimeType, "image/"):
		return imageBlock(r.MimeType, r.Blob)
	case r.Blob != "":
		return llm.TextBlock(fmt.Sprintf("[binary resource %s omitted: %s]", r.URI, r.MimeType))
	default:
		return llm.TextBlock(fmt.Sprintf("Resource %s:\n%s", r.URI, r.Text))
	}
}

func imageBlock(mediaType, data string) llm.Block {
	return llm.Block{Type: llm.BlockImage, Source: &llm.Source{Type: "base64", MediaType: mediaType, Data: data}}
}

// compactJSON returns raw without insignificant space, or raw as is when it
// is not valid JSON.
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}
