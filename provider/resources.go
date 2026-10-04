package truenas

import (
	_ "embed"

	pf "github.com/pulumi/pulumi-terraform-bridge/v3/pkg/pf/tfbridge"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfbridge/tokens"
	shim "github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfshim"
	upstream "github.com/truenas/terraform-provider-truenas/shim"

	"github.com/jetersen/pulumi-truenas/provider/pkg/version"
)

//go:embed cmd/pulumi-resource-truenas/bridge-metadata.json
var metadata []byte

func Provider() tfbridge.ProviderInfo {
	secret := true
	info := tfbridge.ProviderInfo{
		P:    pf.ShimProvider(upstream.NewProvider()),
		Name: "truenas", Version: version.Version,
		DisplayName: "TrueNAS", Publisher: "jetersen",
		Description: "A community Pulumi provider for TrueNAS, bridged from the official Terraform provider.",
		License:     "MPL-2.0", GitHubOrg: "truenas",
		Homepage:          "https://github.com/jetersen/pulumi-truenas",
		Repository:        "https://github.com/jetersen/pulumi-truenas",
		PluginDownloadURL: "github://api.github.com/jetersen/pulumi-truenas",
		MetadataInfo:      tfbridge.NewProviderMetadata(metadata),
		Resources: map[string]*tfbridge.ResourceInfo{
			"truenas_certificate": {Fields: map[string]*tfbridge.SchemaInfo{
				"certificate": {CSharpName: "CertificatePem"},
			}},
			"truenas_app": {Fields: map[string]*tfbridge.SchemaInfo{
				"values":                       {Secret: &secret},
				"custom_compose_config_string": {Secret: &secret},
			}},
		},
		JavaScript: &tfbridge.JavaScriptInfo{
			PackageName: "@jetersen/pulumi-truenas", RespectSchemaVersion: true,
		},
		Python: &tfbridge.PythonInfo{
			PackageName: "jetersen_pulumi_truenas", RespectSchemaVersion: true,
			PyProject: struct{ Enabled bool }{true},
		},
		Golang: &tfbridge.GolangInfo{
			ImportBasePath:       "github.com/jetersen/pulumi-truenas/sdk/go/truenas",
			ModulePath:           "github.com/jetersen/pulumi-truenas/sdk",
			RespectSchemaVersion: true, GenerateResourceContainerTypes: true,
		},
		CSharp: &tfbridge.CSharpInfo{
			RootNamespace: "Jetersen.Pulumi", Namespaces: map[string]string{"truenas": "TrueNas"},
			RespectSchemaVersion: true,
			PackageReferences:    map[string]string{"Pulumi": "3.114.1"},
		},
	}
	info.MustComputeTokens(tokens.SingleModule("truenas_", "index", tokens.MakeStandard("truenas")))
	// Pulumi resource IDs are strings; many TrueNAS resources use integer IDs.
	info.P.ResourcesMap().Range(func(name string, r shim.Resource) bool {
		id, ok := r.Schema().GetOk("id")
		if ok && id.Type() == shim.TypeInt {
			mapping := info.Resources[name]
			if mapping.Fields == nil {
				mapping.Fields = map[string]*tfbridge.SchemaInfo{}
			}
			mapping.Fields["id"] = &tfbridge.SchemaInfo{Type: "string"}
		}
		return true
	})
	return info
}
