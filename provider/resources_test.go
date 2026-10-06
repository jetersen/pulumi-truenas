package truenas

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	shim "github.com/pulumi/pulumi-terraform-bridge/v3/pkg/tfshim"
	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
)

func TestResourceMappings(t *testing.T) {
	p := Provider()
	p.P.ResourcesMap().Range(func(name string, r shim.Resource) bool {
		m := p.Resources[name]
		if m == nil || m.Tok == "" {
			t.Fatalf("resource %s is unmapped", name)
		}
		if id, ok := r.Schema().GetOk("id"); ok && id.Type() == shim.TypeInt {
			if m.Fields["id"] == nil || m.Fields["id"].Type != "string" {
				t.Errorf("integer ID not mapped: %s", name)
			}
		}
		return true
	})
}

func TestGeneratedSDKContract(t *testing.T) {
	b, err := os.ReadFile("cmd/pulumi-resource-truenas/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s schema.PackageSpec
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Resources) != len(Provider().Resources) {
		t.Fatal("schema is missing mapped resources; regenerate it")
	}
	app := s.Resources["truenas:index/app:App"]
	if _, ok := app.InputProperties["compose"]; !ok {
		t.Fatal("missing structured Compose input")
	}
	if app.InputProperties["compose"].Secret {
		t.Fatal("structured Compose must support field-level secrets")
	}
	if _, ok := app.InputProperties["composeSensitivePaths"]; !ok {
		t.Fatal("missing explicit secret paths")
	}
	for _, name := range []string{"values", "customComposeConfigString"} {
		if !app.InputProperties[name].Secret || !app.Properties[name].Secret {
			t.Errorf("%s must be secret in inputs and outputs", name)
		}
	}
	for _, name := range []string{"customComposeConfigStringWo", "customComposeConfigStringWoVersion"} {
		if _, ok := app.InputProperties[name]; !ok {
			t.Errorf("missing upstream Compose overlay field %s", name)
		}
	}
	for token, r := range s.Resources {
		for name, p := range r.InputProperties {
			if strings.HasSuffix(name, "Wo") {
				if !p.Secret || !strings.Contains(p.Description, "Pulumi stores this input encrypted in state") {
					t.Errorf("%s.%s must be secret and document Pulumi state behavior", token, name)
				}
			}
		}
	}
	var csharp struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(s.Resources["truenas:index/certificate:Certificate"].Properties["certificate"].Language["csharp"], &csharp); err != nil {
		t.Fatal(err)
	}
	if csharp.Name != "CertificatePem" {
		t.Fatal("C# Certificate naming collision")
	}
	if s.PluginDownloadURL != "github://api.github.com/jetersen/pulumi-truenas" {
		t.Fatal("provider must download from this repository")
	}
	if !s.Provider.InputProperties["apiKey"].Secret {
		t.Fatal("API key must remain secret")
	}
}
