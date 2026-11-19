package mcp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/aswin-kevin/orchestrix/llm"
	"github.com/aswin-kevin/orchestrix/middleware/filesystem"
	"github.com/aswin-kevin/orchestrix/tool"
)

// imageMediaTypes are the image types passed to the model as images, as
// http.DetectContentType names them.
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
// takes images, the data is valid base64 (padded or not), the decoded bytes
// are at most filesystem.MaxMediaBytes and http.DetectContentType finds one
// of imageMediaTypes in them. The block carries the sniffed type, which wins
// over a declared mediaType that disagrees, and the data re-encoded as
// padded base64. Anything else becomes an "[image omitted: …]" note, so one
// bad block never reaches the provider and poisons the thread.
func imageBlock(mediaType, data string, profile llm.Profile) llm.Block {
	omitted := func(reason string) llm.Block {
		return llm.TextBlock(fmt.Sprintf("[image omitted: %s; %s]", mediaType, reason))
	}
	if !profile.Images {
		return omitted("the model does not take images")
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(data)
	}
	if err != nil {
		return omitted("the data is not valid base64")
	}
	if len(decoded) > filesystem.MaxMediaBytes {
		return omitted(fmt.Sprintf("%d bytes, over the %d-byte limit", len(decoded), filesystem.MaxMediaBytes))
	}
	sniffed, _, _ := mime.ParseMediaType(http.DetectContentType(decoded))
	if !slices.Contains(imageMediaTypes, sniffed) {
		return omitted(fmt.Sprintf("the data is %s, not a PNG, JPEG, GIF or WebP image", sniffed))
	}
	return llm.Block{Type: llm.BlockImage, Source: &llm.Source{
		Type: "base64", MediaType: sniffed, Data: base64.StdEncoding.EncodeToString(decoded),
	}}
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
