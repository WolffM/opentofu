// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mitchellh/cli"
	"github.com/opentofu/opentofu/internal/command/workdir"
	"github.com/opentofu/opentofu/internal/configs/configschema"
	"github.com/opentofu/opentofu/internal/providers"
	"github.com/zclconf/go-cty/cty"
)

func TestQueryCommand_NoQueryFiles(t *testing.T) {
	td := t.TempDir()
	t.Chdir(td)

	ui := new(cli.MockUi)
	c := &QueryCommand{
		Meta: Meta{
			testingOverrides: metaOverridesForProvider(testProvider()),
			Ui:               ui,
			WorkingDir:       workdir.NewDir(td),
		},
	}

	args := []string{}
	if code := c.Run(args); code != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", code, ui.ErrorWriter.String())
	}

	if got := ui.OutputWriter.String(); !strings.Contains(got, "No .tfquery.hcl files found") {
		t.Errorf("expected message about no query files, got: %s", got)
	}
}

func TestQueryCommand_QueryFileWithNoListBlocks(t *testing.T) {
	td := t.TempDir()
	t.Chdir(td)

	// Create a .tfquery.hcl file with no list blocks
	queryContent := "# Empty query file\n"
	if err := os.WriteFile(filepath.Join(td, "query.tfquery.hcl"), []byte(queryContent), 0644); err != nil {
		t.Fatal(err)
	}

	ui := new(cli.MockUi)
	c := &QueryCommand{
		Meta: Meta{
			testingOverrides: metaOverridesForProvider(testProvider()),
			Ui:               ui,
			WorkingDir:       workdir.NewDir(td),
		},
	}

	args := []string{}
	if code := c.Run(args); code != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", code, ui.ErrorWriter.String())
	}

	if got := ui.OutputWriter.String(); !strings.Contains(got, "No list blocks found") {
		t.Errorf("expected message about no list blocks, got: %s", got)
	}
}

func TestQueryCommand_HelpText(t *testing.T) {
	c := &QueryCommand{}
	help := c.Help()
	if !strings.Contains(help, "query [options]") {
		t.Errorf("expected help text to contain 'query [options]', got: %s", help)
	}
	if !strings.Contains(help, "-json") {
		t.Errorf("expected help text to mention -json flag, got: %s", help)
	}
}

func TestQueryCommand_Synopsis(t *testing.T) {
	c := &QueryCommand{}
	syn := c.Synopsis()
	if syn == "" {
		t.Error("expected non-empty synopsis")
	}
}

func TestQueryCommand_JSONOutput(t *testing.T) {
	td := t.TempDir()
	t.Chdir(td)

	ui := new(cli.MockUi)
	c := &QueryCommand{
		Meta: Meta{
			testingOverrides: metaOverridesForProvider(testProvider()),
			Ui:               ui,
			WorkingDir:       workdir.NewDir(td),
		},
	}

	args := []string{"-json"}
	if code := c.Run(args); code != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", code, ui.ErrorWriter.String())
	}
}

func TestQueryCommand_ParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{
			name:    "no args",
			args:    []string{},
			wantErr: false,
		},
		{
			name:    "json flag",
			args:    []string{"-json"},
			wantErr: false,
		},
		{
			name:    "var flag",
			args:    []string{"-var", "foo=bar"},
			wantErr: false,
		},
		{
			name:    "var-file flag",
			args:    []string{"-var-file=vars.tfvars"},
			wantErr: false,
		},
		{
			name:    "unknown flag",
			args:    []string{"-unknown-flag"},
			wantErr: true,
		},
		{
			// -generate-config-out is not supported and should fail
			name:    "generate-config-out flag",
			args:    []string{"-generate-config-out=output.tf"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args, diags := parseQueryArgs(tt.args)
			if tt.wantErr && !diags.HasErrors() {
				t.Errorf("expected error but got none")
			}
			if !tt.wantErr && diags.HasErrors() {
				t.Errorf("unexpected error: %s", diags.Err())
			}
			if !tt.wantErr {
				if args.Path == "" {
					t.Errorf("expected default path to be set")
				}
			}
		})
	}
}

func TestListResourceMockProvider(t *testing.T) {
	// Test that a provider implementing ListResource works correctly via the mock
	p := testProvider()

	// Set up a provider that supports list resources
	p.GetProviderSchemaResponse = &providers.GetProviderSchemaResponse{
		Provider: providers.Schema{
			Block: &configschema.Block{},
		},
		ResourceTypes: map[string]providers.Schema{},
		DataSources:   map[string]providers.Schema{},
		ListResourceTypes: map[string]providers.Schema{
			"aws_instance": {
				Block: &configschema.Block{
					Attributes: map[string]*configschema.Attribute{
						"region": {
							Type:     cty.String,
							Optional: true,
						},
					},
				},
			},
		},
		ServerCapabilities: providers.ServerCapabilities{
			ListResources: true,
		},
	}

	p.ListResourceFn = func(req providers.ListResourceRequest) providers.ListResourceResponse {
		return providers.ListResourceResponse{
			Resources: []providers.ListResourceItem{
				{
					DisplayName: "test-instance",
					Identity:    []byte(`{"id": "i-12345"}`),
				},
			},
		}
	}

	resp := p.ListResource(nil, providers.ListResourceRequest{
		TypeName: "aws_instance",
		Config:   cty.EmptyObjectVal,
	})

	if resp.Diagnostics.HasErrors() {
		t.Errorf("expected no errors, got: %s", resp.Diagnostics.Err())
	}

	if len(resp.Resources) != 1 {
		t.Errorf("expected 1 resource, got %d", len(resp.Resources))
	}

	if resp.Resources[0].DisplayName != "test-instance" {
		t.Errorf("expected display name 'test-instance', got %q", resp.Resources[0].DisplayName)
	}
}

