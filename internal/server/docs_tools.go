package server

import (
	"context"
	"encoding/json"

	"github.com/costfluent/costfluent-mcp/internal/costfluent"
	"github.com/costfluent/costfluent-mcp/internal/docs"
)

// NoCapability gates a tool that calls no Public API: it reads Costfluent's public documentation,
// so a token's capabilities widen nothing it returns. Such a tool is served beside the gated ones
// to every token served at least one, and never on its own.
const NoCapability costfluent.Capability = ""

// docsText ends a docs tool's description with what it reads and what it sends.
func docsText(description string) string {
	return description + " Needs no capability: it reads Costfluent's public documentation at " + docs.Origin +
		", which is sent no query, token or cost data."
}

type SearchDocsInput struct {
	Query    string `json:"query" jsonschema:"what to look for, in the product's words: \"budget alerts terraform\", \"connect azure\", \"Kostenstelle\""`
	Language string `json:"language,omitempty" jsonschema:"the docs' language: en or de; en when omitted"`
	Limit    int    `json:"limit,omitempty" jsonschema:"sections to return: 5 when omitted, at most 10"`
}

// SearchDocsResult is the sections that match a query best, best first.
type SearchDocsResult struct {
	Results []docs.Hit `json:"results"`
}

type GetDocPageInput struct {
	URL string `json:"url" jsonschema:"the page or section as search_docs returns it, such as https://docs.costfluent.com/developers/terraform#manage-objects, or its path, such as /de/connect/azure; a #section reads only that section"`
}

// docsIndex is the docs index the handlers read.
func (h *handlers) docsIndex(ctx context.Context) (*docs.Index, error) {
	source := h.docs
	if source == nil {
		source = docs.Shared()
	}
	return source.Index(ctx)
}

func (h *handlers) searchDocs(ctx context.Context, in SearchDocsInput) (SearchDocsResult, error) {
	ix, err := h.docsIndex(ctx)
	if err != nil {
		return SearchDocsResult{}, err
	}
	hits, err := ix.Search(in.Query, in.Language, in.Limit)
	if err != nil {
		return SearchDocsResult{}, err
	}
	return SearchDocsResult{Results: hits}, nil
}

// getDocPage reads a page, cut so that its answer encodes within maxReportBytes, the bound that
// keeps a result's two copies within an assistant's tool result limit: a page's markdown escapes
// to more than its bytes, and its section list comes on top.
func (h *handlers) getDocPage(ctx context.Context, in GetDocPageInput) (*docs.Page, error) {
	ix, err := h.docsIndex(ctx)
	if err != nil {
		return nil, err
	}
	limit := docs.DefaultPageBytes
	for {
		page, err := ix.Page(in.URL, limit)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			return nil, err
		}
		over := len(encoded) - maxReportBytes
		if over <= 0 || limit == 0 {
			return page, nil
		}
		limit = max(0, limit-over)
	}
}
