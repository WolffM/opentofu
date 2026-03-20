// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/opentofu/opentofu/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

// QueryCommand implements the "tofu query" command, which queries existing
// infrastructure resources using list blocks defined in .tfquery.hcl files.
type QueryCommand struct {
	Meta
}

func (c *QueryCommand) Run(rawArgs []string) int {
	ctx := c.CommandContext()

	args, diags := parseQueryArgs(rawArgs)
	if diags.HasErrors() {
		c.showDiagnostics(diags)
		c.Ui.Error(c.Help())
		return 1
	}

	return c.run(ctx, args)
}

type queryArgs struct {
	// Path is the directory to search for .tfquery.hcl files.
	Path string

	// JSONOutput controls whether output is produced in JSON format.
	JSONOutput bool

	// NoColor disables colorized output.
	NoColor bool

	// Vars contains variable values to pass to the query configuration.
	Vars []string

	// VarFiles contains filenames of variable files to load.
	VarFiles []string
}

func parseQueryArgs(rawArgs []string) (queryArgs, tfdiags.Diagnostics) {
	var args queryArgs
	var diags tfdiags.Diagnostics

	args.Path = "." // default to current directory

	for i := 0; i < len(rawArgs); i++ {
		arg := rawArgs[i]
		switch {
		case arg == "-json":
			args.JSONOutput = true
		case arg == "-no-color":
			args.NoColor = true
		case strings.HasPrefix(arg, "-var="):
			args.Vars = append(args.Vars, strings.TrimPrefix(arg, "-var="))
		case arg == "-var" && i+1 < len(rawArgs):
			i++
			args.Vars = append(args.Vars, rawArgs[i])
		case strings.HasPrefix(arg, "-var-file="):
			args.VarFiles = append(args.VarFiles, strings.TrimPrefix(arg, "-var-file="))
		case arg == "-var-file" && i+1 < len(rawArgs):
			i++
			args.VarFiles = append(args.VarFiles, rawArgs[i])
		case strings.HasPrefix(arg, "-"):
			diags = diags.Append(fmt.Errorf("unknown flag: %s", arg))
		}
	}

	return args, diags
}

type queryResult struct {
	ListResource string                  `json:"list_resource"`
	Items        []queryResultItem       `json:"items"`
	Diagnostics  []queryResultDiagnostic `json:"diagnostics,omitempty"`
}

type queryResultItem struct {
	DisplayName string          `json:"display_name,omitempty"`
	Identity    string          `json:"identity,omitempty"`
	Attributes  json.RawMessage `json:"attributes,omitempty"`
}

type queryResultDiagnostic struct {
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
	Detail   string `json:"detail,omitempty"`
}

func (c *QueryCommand) run(ctx context.Context, args queryArgs) int {
	var diags tfdiags.Diagnostics

	// Find .tfquery.hcl files in the working directory
	dir, err := filepath.Abs(args.Path)
	if err != nil {
		diags = diags.Append(fmt.Errorf("invalid working directory: %w", err))
		c.showDiagnostics(diags)
		return 1
	}

	queryFiles, queryDiags := c.loadQueryFiles(dir)
	diags = diags.Append(queryDiags)
	if diags.HasErrors() {
		c.showDiagnostics(diags)
		return 1
	}

	if len(queryFiles) == 0 {
		c.Ui.Output("No .tfquery.hcl files found in " + dir)
		return 0
	}

	// Collect all list resources from all query files
	var allListResources []*configs.ListResource
	for _, qf := range queryFiles {
		allListResources = append(allListResources, qf.ListResources...)
	}

	if len(allListResources) == 0 {
		c.Ui.Output("No list blocks found in query files.")
		return 0
	}

	// Load the main configuration to get provider configurations
	_, configDiags := c.loadConfig(ctx, dir)
	diags = diags.Append(configDiags)
	if diags.HasErrors() {
		c.showDiagnostics(diags)
		return 1
	}

	// Get provider factories
	providerFactories, err := c.providerFactories()
	if err != nil {
		diags = diags.Append(fmt.Errorf("failed to load provider factories: %w", err))
		c.showDiagnostics(diags)
		return 1
	}

	// Execute each list resource query
	var results []queryResult

	for _, lr := range allListResources {
		result := queryResult{
			ListResource: lr.Addr().String(),
		}

		// Find the provider for this list resource
		providerAddr := lr.Provider
		if providerAddr.IsZero() {
			// Infer provider from the resource type
			providerAddr = addrs.ImpliedProviderForUnqualifiedType(lr.Type)
		}

		factory, ok := providerFactories[providerAddr]
		if !ok {
			result.Diagnostics = append(result.Diagnostics, queryResultDiagnostic{
				Severity: "error",
				Summary:  "Provider not found",
				Detail:   fmt.Sprintf("No provider factory found for %s (required by list.%s.%s)", providerAddr, lr.Type, lr.Name),
			})
			results = append(results, result)
			continue
		}

		provider, err := factory()
		if err != nil {
			result.Diagnostics = append(result.Diagnostics, queryResultDiagnostic{
				Severity: "error",
				Summary:  "Failed to start provider",
				Detail:   fmt.Sprintf("Error starting provider %s: %s", providerAddr, err),
			})
			results = append(results, result)
			continue
		}

		// Get provider schema
		schemaResp := provider.GetProviderSchema(ctx)
		if schemaResp.Diagnostics.HasErrors() {
			for _, d := range schemaResp.Diagnostics {
				result.Diagnostics = append(result.Diagnostics, queryResultDiagnostic{
					Severity: diagSeverityString(d),
					Summary:  d.Description().Summary,
					Detail:   d.Description().Detail,
				})
			}
			provider.Close(ctx) //nolint:errcheck
			results = append(results, result)
			continue
		}

		// Check if the provider supports list resources
		if !schemaResp.ServerCapabilities.ListResources {
			result.Diagnostics = append(result.Diagnostics, queryResultDiagnostic{
				Severity: "error",
				Summary:  "Provider does not support list resources",
				Detail:   fmt.Sprintf("Provider %s does not support the list resources capability. Make sure you are using a provider version that supports this feature.", providerAddr),
			})
			provider.Close(ctx) //nolint:errcheck
			results = append(results, result)
			continue
		}

		// Find the list resource schema
		listSchema, ok := schemaResp.ListResourceTypes[lr.Type]
		if !ok {
			result.Diagnostics = append(result.Diagnostics, queryResultDiagnostic{
				Severity: "error",
				Summary:  "Unknown list resource type",
				Detail:   fmt.Sprintf("Provider %s does not have a list resource type %q.", providerAddr, lr.Type),
			})
			provider.Close(ctx) //nolint:errcheck
			results = append(results, result)
			continue
		}

		// Configure the provider with empty config (provider config from main module is not evaluated here)
		configureResp := provider.ConfigureProvider(ctx, providers.ConfigureProviderRequest{
			Config: cty.EmptyObjectVal,
		})
		if configureResp.Diagnostics.HasErrors() {
			for _, d := range configureResp.Diagnostics {
				result.Diagnostics = append(result.Diagnostics, queryResultDiagnostic{
					Severity: diagSeverityString(d),
					Summary:  d.Description().Summary,
					Detail:   d.Description().Detail,
				})
			}
			provider.Close(ctx) //nolint:errcheck
			results = append(results, result)
			continue
		}

		// Decode the list resource config from the HCL body using the provider schema
		listConfig := cty.EmptyObjectVal
		if lr.Config != nil && listSchema.Block != nil {
			decoded, hclDiags := hcldec.Decode(lr.Config, listSchema.Block.DecoderSpec(), &hcl.EvalContext{})
			if hclDiags.HasErrors() {
				for _, d := range hclDiags {
					result.Diagnostics = append(result.Diagnostics, queryResultDiagnostic{
						Severity: "error",
						Summary:  d.Summary,
						Detail:   d.Detail,
					})
				}
				provider.Close(ctx) //nolint:errcheck
				results = append(results, result)
				continue
			}
			listConfig = decoded
		}

		// Validate the list resource config
		validateResp := provider.ValidateListResourceConfig(ctx, providers.ValidateListResourceConfigRequest{
			TypeName: lr.Type,
			Config:   listConfig,
		})
		if validateResp.Diagnostics.HasErrors() {
			for _, d := range validateResp.Diagnostics {
				result.Diagnostics = append(result.Diagnostics, queryResultDiagnostic{
					Severity: diagSeverityString(d),
					Summary:  d.Description().Summary,
					Detail:   d.Description().Detail,
				})
			}
			provider.Close(ctx) //nolint:errcheck
			results = append(results, result)
			continue
		}

		// Determine limit
		var limit int64 = 100 // default
		if lr.Limit != nil {
			limitVal, limitDiags := lr.Limit.Value(nil)
			if !limitDiags.HasErrors() && limitVal.Type() == cty.Number {
				bf := limitVal.AsBigFloat()
				i64, _ := bf.Int64()
				limit = i64
			}
		}

		// Determine include_resource
		includeResource := false
		if lr.IncludeResource != nil {
			irVal, irDiags := lr.IncludeResource.Value(nil)
			if !irDiags.HasErrors() && irVal.Type() == cty.Bool {
				includeResource = irVal.True()
			}
		}

		// Call ListResource with the decoded config
		listResp := provider.ListResource(ctx, providers.ListResourceRequest{
			TypeName:        lr.Type,
			Config:          listConfig,
			IncludeResource: includeResource,
			Limit:           limit,
		})

		if listResp.Diagnostics.HasErrors() {
			for _, d := range listResp.Diagnostics {
				result.Diagnostics = append(result.Diagnostics, queryResultDiagnostic{
					Severity: diagSeverityString(d),
					Summary:  d.Description().Summary,
					Detail:   d.Description().Detail,
				})
			}
		} else {
			for _, item := range listResp.Resources {
				ri := queryResultItem{
					DisplayName: item.DisplayName,
					Identity:    string(item.Identity),
				}
				if item.Resource != cty.NilVal {
					jsonBytes, jsonErr := ctyjson.Marshal(item.Resource, item.Resource.Type())
					if jsonErr == nil {
						ri.Attributes = json.RawMessage(jsonBytes)
					}
				}
				result.Items = append(result.Items, ri)
			}
		}

		provider.Close(ctx) //nolint:errcheck
		results = append(results, result)
	}

	// Output results
	if args.JSONOutput {
		type allResults struct {
			Results []queryResult `json:"results"`
		}
		output, err := json.MarshalIndent(allResults{Results: results}, "", "  ")
		if err != nil {
			diags = diags.Append(fmt.Errorf("failed to serialize results to JSON: %w", err))
			c.showDiagnostics(diags)
			return 1
		}
		c.Ui.Output(string(output))
	} else {
		c.printQueryResults(results)
	}

	if diags.HasErrors() {
		c.showDiagnostics(diags)
		return 1
	}

	return 0
}

func (c *QueryCommand) loadQueryFiles(dir string) ([]*configs.QueryFile, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	var queryFiles []*configs.QueryFile

	entries, err := os.ReadDir(dir)
	if err != nil {
		diags = diags.Append(fmt.Errorf("failed to read directory %q: %w", dir, err))
		return nil, diags
	}

	parser := configs.NewParser(nil)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".tfquery.hcl") {
			fullPath := filepath.Join(dir, name)
			qf, qfDiags := parser.LoadQueryFile(fullPath)
			if qfDiags.HasErrors() {
				diags = diags.Append(fmt.Errorf("error parsing %s: %s", name, qfDiags.Error()))
			}
			if qf != nil {
				queryFiles = append(queryFiles, qf)
			}
		}
	}

	return queryFiles, diags
}

func (c *QueryCommand) printQueryResults(results []queryResult) {
	for _, r := range results {
		c.Ui.Output(fmt.Sprintf("\nResults for %s:", r.ListResource))
		for _, d := range r.Diagnostics {
			c.Ui.Error(fmt.Sprintf("  [%s] %s: %s", d.Severity, d.Summary, d.Detail))
		}
		for _, item := range r.Items {
			if item.DisplayName != "" {
				c.Ui.Output(fmt.Sprintf("  - %s (%s)", item.DisplayName, item.Identity))
			} else {
				c.Ui.Output(fmt.Sprintf("  - %s", item.Identity))
			}
		}
		if len(r.Items) == 0 && len(r.Diagnostics) == 0 {
			c.Ui.Output("  (no results)")
		}
	}
}

func diagSeverityString(d tfdiags.Diagnostic) string {
	if d.Severity() == tfdiags.Error {
		return "error"
	}
	return "warning"
}

func (c *QueryCommand) Synopsis() string {
	return "Query existing infrastructure according to list blocks in .tfquery.hcl files"
}

func (c *QueryCommand) Help() string {
	helpText := `
Usage: tofu [global options] query [options]

  Queries existing infrastructure for resources according to the list blocks
  defined in .tfquery.hcl files in the current directory. This helps find
  resources that are not yet managed by OpenTofu so that they can be imported.

  Providers that support list resources will return a list of existing resource
  instances matching the filter configuration in each list block.

Options:

  -json                    Produce output in a machine-readable JSON format.

  -no-color                If specified, output will not contain any color.

  -var 'foo=bar'           Set a value for one of the input variables in the
                           query file configuration. Use this option more than
                           once to set more than one variable.

  -var-file=filename       Load variable values from the given file.
`
	return strings.TrimSpace(helpText)
}
