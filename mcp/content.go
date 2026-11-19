package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/tool"
)

// imageMediaTypes are the image types passed to the model as images.
var imageMediaTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

// toolResult maps a tool call's result to the agent's: one block per content
// block; when there is none, the structured content as compact JSON. Images
// pass only as imageBlock allows for the model's profile.
func toolResult(res *CallToolResult, profile llm.Profile) tool.Result {
	var out tool.Result
	for _, c := range res.Content {
		out.Content = append(out.Content, contentBlock(c, profile))
	}
	if len(out.Content) == 0 && len(res.StructuredContent) > 0 {
		out.Content = []llm.Block{llm.TextBlock(compactJSON(res.StructuredContent))}
	}
	out.IsError = res.IsError
	return out
}

// contentBlock maps one content block. Images become image blocks (see
// imageBlock); audio and unknown kinds become a note.
func contentBlock(c Content, profile llm.Profile) llm.Block {
	switch c.Type {
	case "text":
		return llm.TextBlock(c.Text)
	case "image":
		return imageBlock(c.MimeType, c.Data, profile)
	case "audio":
		return llm.TextBlock(fmt.Sprintf("[audio content omitted: %s]", c.MimeType))
	case "resource":
		if c.Resource == nil {
			return llm.TextBlock("[empty resource omitted]")
		}
		return resourceBlock(*c.Resource, profile)
	case "resource_link":
		return llm.TextBlock(fmt.Sprintf("Resource link: %s (%s)", c.URI, c.Name))
	default:
		return llm.TextBlock(fmt.Sprintf("[%q content omitted]", c.Type))
	}
}

// resourceBlock maps the content of a resource: its text, an image blob as
// an image (see imageBlock), any other blob as a note.
func resourceBlock(r ResourceContent, profile llm.Profile) llm.Block {
	switch {
	case r.Blob != "" && strings.HasPrefix(r.MimeType, "image/"):
		return imageBlock(r.MimeType, r.Blob, profile)
	case r.Blob != "":
		return llm.TextBlock(fmt.Sprintf("[binary resource %s omitted: %s]", r.URI, r.MimeType))
	default:
		return llm.TextBlock(fmt.Sprintf("Resource %s:\n%s", r.URI, r.Text))
	}
}

// imageBlock returns base64 image data as an image block when the model
// takes images, the media type is in imageMediaTypes and the decoded size is
// at most filesystem.MaxMediaBytes; otherwise a note saying it was omitted.
func imageBlock(mediaType, data string, profile llm.Profile) llm.Block {
	size := decodedLen(data)
	if !profile.Images || !slices.Contains(imageMediaTypes, mediaType) || size > filesystem.MaxMediaBytes {
		return llm.TextBlock(fmt.Sprintf("[image omitted: %s, %d bytes]", mediaType, size))
	}
	return llm.Block{Type: llm.BlockImage, Source: &llm.Source{Type: "base64", MediaType: mediaType, Data: data}}
}

// decodedLen is the size of the bytes the padded base64 text data encodes.
func decodedLen(data string) int {
	return len(data)/4*3 - (len(data) - len(strings.TrimRight(data, "=")))
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
