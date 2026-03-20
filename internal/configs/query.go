// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package configs

import (
	"github.com/hashicorp/hcl/v2"

	"github.com/opentofu/opentofu/internal/addrs"
)

// QueryFile represents a parsed .tfquery.hcl file, which defines list blocks
// for querying existing infrastructure resources.
type QueryFile struct {
	// ListResources contains the list blocks defined in this query file.
	ListResources []*ListResource

	// Variables contains variable definitions from this query file.
	Variables []*Variable

	// Locals contains local value definitions from this query file.
	Locals []*Local
}

// ListResource represents a "list" block in a query file. It defines a query
// that retrieves existing infrastructure resources from a provider.
type ListResource struct {
	// Type is the list resource type name (provider-defined).
	Type string

	// Name is the local label for this list resource.
	Name string

	// ProviderConfigRef is the reference to the provider configuration to use.
	ProviderConfigRef *ProviderConfigRef

	// Provider is the resolved provider address for this list resource.
	Provider addrs.Provider

	// Config is the HCL body containing the provider-specific filter config.
	Config hcl.Body

	// IncludeResource when true requests the full resource state from the provider.
	IncludeResource hcl.Expression

	// Limit is the maximum number of results to return.
	Limit hcl.Expression

	DeclRange hcl.Range
	TypeRange hcl.Range
}

// Addr returns the address for this list resource relative to its containing module.
func (l *ListResource) Addr() addrs.Resource {
	return addrs.Resource{
		Mode: addrs.ListResourceMode,
		Type: l.Type,
		Name: l.Name,
	}
}

// listResourceBlockSchema is the HCL schema for list blocks in query files.
var listResourceBlockSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{
			Name:     "provider",
			Required: false,
		},
		{
			Name:     "include_resource",
			Required: false,
		},
		{
			Name:     "limit",
			Required: false,
		},
	},
	Blocks: []hcl.BlockHeaderSchema{
		{
			Type: "config",
		},
	},
}

// decodeListResourceBlock decodes a "list" block from a query file.
func decodeListResourceBlock(block *hcl.Block) (*ListResource, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	lr := &ListResource{
		Type:      block.Labels[0],
		Name:      block.Labels[1],
		DeclRange: block.DefRange,
		TypeRange: block.LabelRanges[0],
	}

	content, remain, moreDiags := block.Body.PartialContent(listResourceBlockSchema)
	diags = append(diags, moreDiags...)

	// The remaining body content is used as the provider-specific config
	// (when no explicit "config" block is present)
	configBody := remain

	if attr, exists := content.Attributes["provider"]; exists {
		providerRef, providerRefDiags := decodeProviderConfigRef(attr.Expr, "provider")
		diags = append(diags, providerRefDiags...)
		if !providerRefDiags.HasErrors() {
			lr.ProviderConfigRef = providerRef
		}
	}

	if attr, exists := content.Attributes["include_resource"]; exists {
		lr.IncludeResource = attr.Expr
	}

	if attr, exists := content.Attributes["limit"]; exists {
		lr.Limit = attr.Expr
	}

	// Check for an explicit "config" block
	for _, configBlock := range content.Blocks {
		if configBlock.Type == "config" {
			configBody = configBlock.Body
		}
	}

	lr.Config = configBody

	return lr, diags
}

// queryFileSchema is the top-level HCL schema for .tfquery.hcl files.
var queryFileSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{
			Type:       "list",
			LabelNames: []string{"type", "name"},
		},
		{
			Type:       "variable",
			LabelNames: []string{"name"},
		},
		{
			Type: "locals",
		},
	},
}

// parseQueryFile parses a .tfquery.hcl file body and returns a QueryFile.
func parseQueryFile(body hcl.Body) (*QueryFile, hcl.Diagnostics) {
	var diags hcl.Diagnostics

	content, moreDiags := body.Content(queryFileSchema)
	diags = append(diags, moreDiags...)

	file := &QueryFile{}

	for _, block := range content.Blocks {
		switch block.Type {
		case "list":
			lr, lrDiags := decodeListResourceBlock(block)
			diags = append(diags, lrDiags...)
			if lr != nil {
				file.ListResources = append(file.ListResources, lr)
			}
		case "variable":
			cfg, cfgDiags := decodeVariableBlock(block, false)
			diags = append(diags, cfgDiags...)
			if cfg != nil {
				file.Variables = append(file.Variables, cfg)
			}
		case "locals":
			defs, defsDiags := decodeLocalsBlock(block)
			diags = append(diags, defsDiags...)
			file.Locals = append(file.Locals, defs...)
		}
	}

	return file, diags
}

// LoadQueryFile parses the query file at the given path and returns a QueryFile.
func (p *Parser) LoadQueryFile(path string) (*QueryFile, hcl.Diagnostics) {
	body, diags := p.LoadHCLFile(path)
	if diags.HasErrors() {
		return nil, diags
	}

	qf, moreDiags := parseQueryFile(body)
	diags = append(diags, moreDiags...)

	return qf, diags
}

