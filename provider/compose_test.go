package truenas

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	pf "github.com/pulumi/pulumi-terraform-bridge/v3/pkg/pf/tfbridge"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/plugin"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

func TestComposeProtection(t *testing.T) {
	props := resource.NewPropertyMapFromMap(map[string]any{
		"customApp": true,
		"compose": map[string]any{"services": map[string]any{"dns": map[string]any{
			"image": "example/dns:1", "environment": map[string]any{"PASSWORD": "fixture-password", "DOMAIN": "example.test"},
			"command": []any{"server", "--password=fixture-command"}, "labels": map[string]any{"auth": "fixture-label"},
			"hostname": "private-host", "x-private": map[string]any{"key": "fixture-extension"},
		}}},
		"composeSensitivePaths": []any{"/services/dns/hostname", "/services/dns/environment/PASSWORD"},
	})
	checked, err := composeCheck(context.Background(), props, nil)
	require.NoError(t, err)
	service := checked["compose"].ObjectValue()["services"].ObjectValue()["dns"].ObjectValue()
	require.False(t, service["image"].IsSecret())
	require.True(t, service["hostname"].IsSecret())
	for _, key := range []string{"command", "labels", "x-private"} {
		require.False(t, service[resource.PropertyKey(key)].ContainsSecrets(), key)
	}
	require.True(t, service["environment"].ObjectValue()["PASSWORD"].IsSecret())
	require.False(t, service["environment"].ObjectValue()["DOMAIN"].IsSecret())
	require.False(t, props["compose"].ContainsSecrets(), "must not mutate caller inputs")
	again, err := composeProperties(context.Background(), checked)
	require.NoError(t, err)
	require.True(t, checked.DeepEquals(again))
	unknownPaths := props.Copy()
	unknownPaths["composeSensitivePaths"] = resource.MakeComputed(resource.NewArrayProperty(nil))
	protected, err := composeProperties(context.Background(), unknownPaths)
	require.NoError(t, err)
	require.True(t, protected["compose"].IsSecret(), "unknown policy must protect the whole document")
	// List-style environments are encrypted as one value, including on reordering.
	list := resource.NewObjectProperty(resource.PropertyMap{"environment": resource.NewArrayProperty([]resource.PropertyValue{resource.NewStringProperty("PASSWORD=fixture")})})
	require.True(t, secretAt(list, []string{"environment", "*"}).ObjectValue()["environment"].IsSecret())
	// Invalid YAML errors must not repeat source lines or secret values.
	_, err = canonicalCompose("services: [fixture-password: ")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "fixture-password")
	_, err = canonicalCompose("services: {}\n---\nservices: {}\n")
	require.Error(t, err)
	_, err = pointerParts("/services/~invalid")
	require.Error(t, err)
}

func TestComposeCanonicalFormatting(t *testing.T) {
	a, err := canonicalCompose("services:\n  dns:\n    image: example/dns:1\n    environment:\n      PORT: '53'\nvolumes: {}\n")
	require.NoError(t, err)
	b, err := canonicalCompose("# comment\nvolumes: {}\nservices: {dns: {environment: {PORT: \"53\"}, image: 'example/dns:1'}}\n")
	require.NoError(t, err)
	require.Equal(t, a, b)
}

type fixtureNAS struct {
	mu        sync.Mutex
	document  map[string]any
	writes    int
	reads     int
	failRead  bool
	failWrite bool
}

func (nas *fixtureNAS) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		var req struct {
			ID     any               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if conn.ReadJSON(&req) != nil {
			return
		}
		nas.mu.Lock()
		var result any
		var rpcError any
		switch req.Method {
		case "auth.login_with_api_key":
			result = true
		case "auth.mechanism_choices":
			result = []string{"API_KEY_PLAIN"}
		case "app.create", "app.update":
			if nas.failWrite {
				rpcError = map[string]any{"code": -1, "message": "fixture-sensitive-payload"}
				break
			}
			var payload struct {
				Compose string `json:"custom_compose_config_string"`
			}
			ix := 0
			if req.Method == "app.update" {
				ix = 1
			}
			if json.Unmarshal(req.Params[ix], &payload) != nil || yaml.Unmarshal([]byte(payload.Compose), &nas.document) != nil {
				rpcError = map[string]any{"code": -1, "message": "invalid fixture payload"}
			}
			nas.writes++
			result = map[string]any{}
		case "app.get_instance":
			result = map[string]any{"name": "dns", "id": "dns", "custom_app": true, "state": "RUNNING", "version": "1.0.0", "metadata": map[string]any{"train": ""}}
		case "app.config":
			nas.reads++
			if nas.failRead {
				rpcError = map[string]any{"code": -1, "message": "fixture-sensitive-error"}
			} else {
				result = nas.document
			}
		case "app.delete":
			nas.writes++
			result = true
		default:
			rpcError = map[string]any{"code": -32601, "message": "unsupported fixture method"}
		}
		response := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rpcError != nil {
			response["error"] = rpcError
		} else {
			response["result"] = result
		}
		err = conn.WriteJSON(response)
		nas.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func TestComposeProviderLifecycle(t *testing.T) {
	// All API calls terminate at a local TLS WebSocket fixture. No NAS credentials
	// or production endpoints are read by this test.
	nas := &fixtureNAS{}
	server := httptest.NewTLSServer(http.HandlerFunc(nas.serve))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p, err := pf.NewProvider(ctx, Provider(), pf.ProviderMetadata{PackageSchema: []byte(`{}`)})
	require.NoError(t, err)
	defer p.Close()
	_, err = p.Configure(ctx, plugin.ConfigureRequest{Inputs: resource.NewPropertyMapFromMap(map[string]any{
		"endpoint": "wss" + strings.TrimPrefix(server.URL, "https") + "/api/current", "apiKey": "fixture-api-key", "insecure": true,
	})})
	require.NoError(t, err)
	urn := resource.URN("urn:pulumi:test::compose::truenas:index/app:App::dns")
	news := resource.NewPropertyMapFromMap(map[string]any{"name": "dns", "customApp": true, "running": true, "compose": map[string]any{
		"services": map[string]any{"dns": map[string]any{"image": "example/dns:1", "environment": map[string]any{"PASSWORD": "fixture-old"}}},
	}})
	news["composeSensitivePaths"] = resource.NewArrayProperty([]resource.PropertyValue{
		resource.NewStringProperty("/services/dns/environment/PASSWORD"),
		resource.NewStringProperty("/services/dns/environment/NEW_TOKEN"),
	})
	checked, err := p.Check(ctx, plugin.CheckRequest{URN: urn, News: news})
	require.NoError(t, err)
	require.Empty(t, checked.Failures)
	created, err := p.Create(ctx, plugin.CreateRequest{URN: urn, Properties: checked.Properties})
	require.NoError(t, err)
	require.Equal(t, resource.ID("dns"), created.ID)
	require.False(t, created.Properties["customComposeConfigString"].HasValue(), "do not duplicate plaintext serialization")
	service := created.Properties["compose"].ObjectValue()["services"].ObjectValue()["dns"].ObjectValue()
	require.True(t, service["environment"].ObjectValue()["PASSWORD"].IsSecret())
	require.False(t, service["image"].IsSecret())
	read, err := p.Read(ctx, plugin.ReadRequest{URN: urn, ID: created.ID, Inputs: cloneProperties(t, checked.Properties), State: created.Properties})
	require.NoError(t, err)
	diff, err := p.Diff(ctx, plugin.DiffRequest{URN: urn, ID: created.ID, OldInputs: read.Inputs, OldOutputs: read.Outputs, NewInputs: checked.Properties})
	require.NoError(t, err)
	require.Equal(t, plugin.DiffNone, diff.Changes, "unchanged config should not diff")
	nas.mu.Lock()
	dns := nas.document["services"].(map[string]any)["dns"].(map[string]any)
	dns["image"] = "example/dns:2"
	dns["environment"].(map[string]any)["PASSWORD"] = "fixture-drift"
	dns["environment"].(map[string]any)["NEW_TOKEN"] = "fixture-new"
	nas.mu.Unlock()
	drift, err := p.Read(ctx, plugin.ReadRequest{URN: urn, ID: created.ID, Inputs: cloneProperties(t, read.Inputs), State: read.Outputs})
	require.NoError(t, err)
	service = drift.Outputs["compose"].ObjectValue()["services"].ObjectValue()["dns"].ObjectValue()
	require.Equal(t, "example/dns:2", service["image"].StringValue())
	require.True(t, service["environment"].ObjectValue()["NEW_TOKEN"].IsSecret(), "explicit paths must protect newly discovered fields")
	diff, err = p.Diff(ctx, plugin.DiffRequest{URN: urn, ID: created.ID, OldInputs: drift.Inputs, OldOutputs: drift.Outputs, NewInputs: checked.Properties})
	require.NoError(t, err)
	require.Equal(t, plugin.DiffSome, diff.Changes)
	require.Contains(t, diff.ChangedKeys, resource.PropertyKey("compose"))
	require.Empty(t, diff.ReplaceKeys, "drift must not replace the app")
	nas.mu.Lock()
	require.Equal(t, 1, nas.writes, "refresh and diff must be read-only")
	nas.mu.Unlock()
	require.Equal(t, "example/dns:1", checked.Properties["compose"].ObjectValue()["services"].ObjectValue()["dns"].ObjectValue()["image"].StringValue(), "checked inputs mutated")
	updated, err := p.Update(ctx, plugin.UpdateRequest{URN: urn, ID: created.ID, OldInputs: drift.Inputs, OldOutputs: drift.Outputs, NewInputs: checked.Properties})
	require.NoError(t, err)
	require.Equal(t, "example/dns:1", updated.Properties["compose"].ObjectValue()["services"].ObjectValue()["dns"].ObjectValue()["image"].StringValue())
	nas.mu.Lock()
	nas.failWrite = true
	nas.mu.Unlock()
	rejected := cloneProperties(t, checked.Properties)
	rejected["compose"].ObjectValue()["services"].ObjectValue()["dns"].ObjectValue()["image"] = resource.NewStringProperty("example/dns:3")
	_, err = p.Update(ctx, plugin.UpdateRequest{URN: urn, ID: created.ID, OldInputs: checked.Properties, OldOutputs: updated.Properties, NewInputs: rejected})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "fixture-sensitive-payload")
	nas.mu.Lock()
	nas.failRead = true
	nas.mu.Unlock()
	_, err = p.Read(ctx, plugin.ReadRequest{URN: urn, ID: created.ID, Inputs: cloneProperties(t, checked.Properties), State: updated.Properties})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "fixture-sensitive-error")
}

func TestComposeCLIPreview(t *testing.T) {
	if os.Getenv("TRUENAS_COMPOSE_CLI_TEST") != "1" {
		t.Skip("run make test-compose-preview for isolated CLI coverage")
	}
	for _, runtime := range []string{"yaml", "dotnet"} {
		t.Run(runtime, func(t *testing.T) { testComposeCLIPreview(t, runtime, false) })
		t.Run(runtime+"-overlay", func(t *testing.T) { testComposeCLIPreview(t, runtime, true) })
	}
}

func testComposeCLIPreview(t *testing.T, runtime string, overlay bool) {
	if os.Getenv("TRUENAS_COMPOSE_CLI_TEST") != "1" {
		t.Skip("run make test-compose-preview for isolated CLI coverage")
	}
	require.FileExists(t, "../bin/pulumi-resource-truenas")
	nas := &fixtureNAS{}
	server := httptest.NewTLSServer(http.HandlerFunc(nas.serve))
	defer server.Close()
	dir := t.TempDir()
	binary, err := filepath.Abs("../bin")
	require.NoError(t, err)
	env := []string{}
	for _, v := range os.Environ() {
		key, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(key, "PULUMI_") || strings.HasPrefix(key, "TRUENAS_") {
			continue
		}
		env = append(env, v)
	}
	env = append(env, "PULUMI_HOME="+filepath.Join(dir, "home"), "PULUMI_CONFIG_PASSPHRASE=fixture-passphrase", "PULUMI_SKIP_UPDATE_CHECK=true")
	runCLI := func(t *testing.T, args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "pulumi", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "Pulumi %v failed: %s", args, string(out))
		return string(out)
	}
	run := func(args ...string) string { return runCLI(t, args...) }
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "state"), 0700))
	run("login", "file://"+filepath.Join(dir, "state"))
	version := os.Getenv("VERSION")
	if version == "" {
		version = "0.1.0"
	}
	run("plugin", "install", "resource", "truenas", version, "--file", binary)
	project := fmt.Sprintf(`name: compose-fixture
runtime: yaml
resources:
  nas:
    type: pulumi:providers:truenas
    options:
      version: %s
    properties:
      endpoint: %s
      apiKey: fixture-api-key
      insecure: true
  dns:
    type: truenas:index/app:App
    options:
      provider: ${nas}
    properties:
      name: dns
      customApp: true
      running: true
      composeSensitivePaths: [/services/dns/environment/NEW_TOKEN]
      compose:
        services:
          dns:
            image: example/dns:1
            environment:
              PASSWORD:
                fn::secret: fixture-password
              DOMAIN: public.example
`, version, "wss"+strings.TrimPrefix(server.URL, "https")+"/api/current")
	if runtime == "dotnet" {
		project = "name: compose-fixture\nruntime: dotnet\n"
		sdk, err := filepath.Abs("../sdk/dotnet/Jetersen.Pulumi.TrueNas.csproj")
		require.NoError(t, err)
		csproj := fmt.Sprintf(`<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><OutputType>Exe</OutputType><TargetFramework>net10.0</TargetFramework><ImplicitUsings>enable</ImplicitUsings></PropertyGroup><ItemGroup><ProjectReference Include="%s" /></ItemGroup></Project>`, sdk)
		program := fmt.Sprintf(`using Pulumi;
using TrueNas = Jetersen.Pulumi.TrueNas;
return await Deployment.RunAsync(() => {
 var nas = new TrueNas.Provider("nas",new TrueNas.ProviderArgs { Endpoint = "%s", ApiKey = Output.CreateSecret("fixture-api-key"), Insecure = true });
 var app = new TrueNas.App("dns",new TrueNas.AppArgs {
  Name = "dns", CustomApp = true, Running = true,
  ComposeSensitivePaths = { "/services/dns/environment/NEW_TOKEN" },
  Compose = new Dictionary<string,object> {
   ["services"] = new Dictionary<string,object> {
    ["dns"] = new Dictionary<string,object> {
     ["image"] = "example/dns:1",
     ["environment"] = new Dictionary<string,object> { ["PASSWORD"] = Output.CreateSecret("fixture-password"), ["DOMAIN"] = "public.example" }
    }
   }
  }
 },new CustomResourceOptions { Provider = nas });
 return new Dictionary<string,object>();
});`, "wss"+strings.TrimPrefix(server.URL, "https")+"/api/current")
		if overlay {
			program = strings.ReplaceAll(program, `["PASSWORD"] = Output.CreateSecret("fixture-password"), `, "")
			program = strings.Replace(program, "  Compose =", `  CustomComposeConfigStringWo = Output.CreateSecret("""{"services":{"dns":{"environment":{"PASSWORD":"fixture-password"}}}}"""),
  CustomComposeConfigStringWoVersion = 1,
  Compose =`, 1)
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "Fixture.csproj"), []byte(csproj), 0600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "Program.cs"), []byte(program), 0600))
	}
	if overlay && runtime == "yaml" {
		project = strings.Replace(project, "              PASSWORD:\n                fn::secret: fixture-password\n", "", 1)
		project = strings.Replace(project, "      compose:\n", `      customComposeConfigStringWo:
        fn::secret: '{"services":{"dns":{"environment":{"PASSWORD":"fixture-password"}}}}'
      customComposeConfigStringWoVersion: 1
      compose:
`, 1)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Pulumi.yaml"), []byte(project), 0600))
	run("stack", "init", "test", "--non-interactive")
	t.Run("secret-create-preview", func(t *testing.T) {
		preview := runCLI(t, "preview", "--diff", "--non-interactive", "--color", "never")
		require.Contains(t, preview, "example/dns:1")
		require.Contains(t, preview, "public.example")
		require.Contains(t, preview, "[secret]")
		for _, secret := range []string{"fixture-password", "fixture-api-key"} {
			require.NotContains(t, preview, secret)
		}
	})
	run("up", "--yes", "--skip-preview", "--non-interactive")
	exported := run("stack", "export")
	require.NotContains(t, exported, "fixture-password", "secret must be encrypted in persisted state")
	require.NotContains(t, exported, "fixture-api-key")
	t.Run("secret-update-preview", func(t *testing.T) {
		// Exercise normal input changes, not just refresh drift. PASSWORD is
		// protected solely by fn::secret / Output.CreateSecret, not a path rule.
		programPath := filepath.Join(dir, "Pulumi.yaml")
		if runtime == "dotnet" {
			programPath = filepath.Join(dir, "Program.cs")
		}
		original, err := os.ReadFile(programPath)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, os.WriteFile(programPath, original, 0600)) })
		changed := strings.ReplaceAll(string(original), "fixture-password", "fixture-rotated-password")
		changed = strings.ReplaceAll(changed, "example/dns:1", "example/dns:3")
		if overlay {
			changed = strings.ReplaceAll(changed, "customComposeConfigStringWoVersion: 1", "customComposeConfigStringWoVersion: 2")
			changed = strings.ReplaceAll(changed, "CustomComposeConfigStringWoVersion = 1", "CustomComposeConfigStringWoVersion = 2")
		}
		require.NoError(t, os.WriteFile(programPath, []byte(changed), 0600))
		preview := runCLI(t, "preview", "--diff", "--non-interactive", "--color", "never")
		require.Contains(t, preview, "example/dns:1")
		require.Contains(t, preview, "example/dns:3")
		if !overlay {
			require.Contains(t, preview, "[secret]")
		} else {
			require.Contains(t, preview, "customComposeConfigStringWoVersion")
		}
		for _, secret := range []string{"fixture-password", "fixture-rotated-password", "fixture-api-key"} {
			require.NotContains(t, preview, secret)
		}
		nas.mu.Lock()
		defer nas.mu.Unlock()
		require.Equal(t, 1, nas.writes, "preview must not deploy the rotated secret")
	})
	nas.mu.Lock()
	dns := nas.document["services"].(map[string]any)["dns"].(map[string]any)
	dns["image"] = "example/dns:2"
	dns["environment"].(map[string]any)["PASSWORD"] = "fixture-drift-password"
	dns["environment"].(map[string]any)["NEW_TOKEN"] = "fixture-new-token"
	dns["environment"].(map[string]any)["DOMAIN"] = "manual.example"
	nas.mu.Unlock()
	preview := run("preview", "--refresh", "--diff", "--non-interactive", "--color", "never")
	require.Contains(t, preview, "example/dns:2")
	require.Contains(t, preview, "example/dns:1")
	if !overlay {
		require.Contains(t, preview, "[secret]")
	}
	require.Contains(t, preview, "public.example")
	require.Contains(t, preview, "manual.example")
	for _, s := range []string{"fixture-password", "fixture-drift-password", "fixture-new-token"} {
		require.NotContains(t, preview, s)
	}
	run("refresh", "--yes", "--non-interactive")
	refreshed := run("stack", "export")
	for _, s := range []string{"fixture-password", "fixture-drift-password", "fixture-new-token"} {
		require.NotContains(t, refreshed, s)
	}
	if overlay {
		var state struct {
			Deployment struct {
				Resources []struct {
					Type    string         `json:"type"`
					Inputs  map[string]any `json:"inputs"`
					Outputs map[string]any `json:"outputs"`
				} `json:"resources"`
			} `json:"deployment"`
		}
		for _, snapshot := range []string{exported, refreshed} {
			require.NoError(t, json.Unmarshal([]byte(snapshot), &state))
			found := false
			for _, r := range state.Deployment.Resources {
				if r.Type != "truenas:index/app:App" {
					continue
				}
				found = true
				// Pulumi stores the encrypted input after creation, even though
				// the bridge currently drops it on refresh. Outputs stay projected.
				if snapshot == exported {
					secret, ok := r.Inputs["customComposeConfigStringWo"].(map[string]any)
					require.True(t, ok)
					require.Contains(t, secret, "ciphertext")
				}
				require.NotContains(t, r.Outputs, "customComposeConfigStringWo")
				b, err := json.Marshal(r.Outputs["compose"])
				require.NoError(t, err)
				require.NotContains(t, string(b), "PASSWORD")
				require.NotContains(t, string(b), "NEW_TOKEN")
			}
			require.True(t, found)
		}
	}
	nas.mu.Lock()
	require.Equal(t, 1, nas.writes)
	nas.mu.Unlock()
	t.Logf("Verified CLI preview:\n%s", preview)
}

// Model the RPC boundary: the bridge can reconcile old input maps in place.
func cloneProperties(t *testing.T, props resource.PropertyMap) resource.PropertyMap {
	t.Helper()
	wire, err := plugin.MarshalProperties(props, plugin.MarshalOptions{KeepSecrets: true, KeepUnknowns: true})
	require.NoError(t, err)
	out, err := plugin.UnmarshalProperties(wire, plugin.MarshalOptions{KeepSecrets: true, KeepUnknowns: true})
	require.NoError(t, err)
	return out
}

func TestComposeLegacyAndImport(t *testing.T) {
	nas := &fixtureNAS{document: map[string]any{"services": map[string]any{"dns": map[string]any{
		"image": "example/dns:1", "environment": []any{"PASSWORD=fixture-password"}, "volumes": []any{"data:/data"},
	}}, "volumes": map[string]any{"data": nil}}}
	server := httptest.NewTLSServer(http.HandlerFunc(nas.serve))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p, err := pf.NewProvider(ctx, Provider(), pf.ProviderMetadata{PackageSchema: []byte(`{}`)})
	require.NoError(t, err)
	defer p.Close()
	_, err = p.Configure(ctx, plugin.ConfigureRequest{Inputs: resource.NewPropertyMapFromMap(map[string]any{
		"endpoint": "wss" + strings.TrimPrefix(server.URL, "https") + "/api/current", "apiKey": "fixture-api-key", "insecure": true,
	})})
	require.NoError(t, err)
	urn := resource.URN("urn:pulumi:test::compose::truenas:index/app:App::dns")
	imported, err := p.Read(ctx, plugin.ReadRequest{URN: urn, ID: "dns"})
	require.NoError(t, err)
	require.Equal(t, resource.ID("dns"), imported.ID)
	require.True(t, imported.Outputs["customComposeConfigString"].IsSecret())
	b, err := json.Marshal(nas.document)
	require.NoError(t, err)
	raw := resource.NewPropertyMapFromMap(map[string]any{"name": "dns", "customApp": true, "running": true, "customComposeConfigString": string(b)})
	checked, err := p.Check(ctx, plugin.CheckRequest{URN: urn, News: raw})
	require.NoError(t, err)
	require.Empty(t, checked.Failures)
	created, err := p.Create(ctx, plugin.CreateRequest{URN: urn, Properties: checked.Properties})
	require.NoError(t, err)
	read, err := p.Read(ctx, plugin.ReadRequest{URN: urn, ID: "dns", Inputs: cloneProperties(t, checked.Properties), State: created.Properties})
	require.NoError(t, err)
	require.True(t, read.Outputs["customComposeConfigString"].IsSecret())
	raw["customComposeConfigString"] = resource.NewStringProperty("# formatting only\nvolumes: {data: null}\nservices:\n  dns:\n    volumes: ['data:/data']\n    environment: ['PASSWORD=fixture-password']\n    image: example/dns:1\n")
	reformatted, err := p.Check(ctx, plugin.CheckRequest{URN: urn, News: raw})
	require.NoError(t, err)
	diff, err := p.Diff(ctx, plugin.DiffRequest{URN: urn, ID: "dns", OldInputs: read.Inputs, OldOutputs: read.Outputs, NewInputs: reformatted.Properties})
	require.NoError(t, err)
	require.Equal(t, plugin.DiffNone, diff.Changes)
	// Switch representations without recreating or redeploying an unchanged app.
	structured := resource.NewPropertyMapFromMap(map[string]any{"name": "dns", "customApp": true, "running": true, "compose": nas.document})
	desired, err := p.Check(ctx, plugin.CheckRequest{URN: urn, News: structured})
	require.NoError(t, err)
	diff, err = p.Diff(ctx, plugin.DiffRequest{URN: urn, ID: "dns", OldInputs: read.Inputs, OldOutputs: read.Outputs, NewInputs: desired.Properties})
	require.NoError(t, err)
	require.Empty(t, diff.ReplaceKeys)
	_, err = p.Update(ctx, plugin.UpdateRequest{URN: urn, ID: "dns", OldInputs: read.Inputs, OldOutputs: read.Outputs, NewInputs: desired.Properties})
	require.NoError(t, err)
	nas.mu.Lock()
	require.Equal(t, 1, nas.writes, "changing representation must not redeploy unchanged Compose")
	nas.mu.Unlock()
}

func TestComposeOverlay(t *testing.T) {
	for _, structured := range []bool{false, true} {
		name := "string"
		if structured {
			name = "structured"
		}
		t.Run(name, func(t *testing.T) { testComposeOverlay(t, structured) })
	}
}

func testComposeOverlay(t *testing.T, structured bool) {
	nas := &fixtureNAS{}
	server := httptest.NewTLSServer(http.HandlerFunc(nas.serve))
	defer server.Close()
	ctx := context.Background()
	p, err := pf.NewProvider(ctx, Provider(), pf.ProviderMetadata{PackageSchema: []byte(`{}`)})
	require.NoError(t, err)
	defer p.Close()
	_, err = p.Configure(ctx, plugin.ConfigureRequest{Inputs: resource.NewPropertyMapFromMap(map[string]any{
		"endpoint": "wss" + strings.TrimPrefix(server.URL, "https") + "/api/current", "apiKey": "fixture-key", "insecure": true,
	})})
	require.NoError(t, err)
	urn := resource.URN("urn:pulumi:test::compose::truenas:index/app:App::dns")
	news := resource.NewPropertyMapFromMap(map[string]any{
		"name": "dns", "customApp": true, "running": true,
		"compose":                            map[string]any{"services": map[string]any{"dns": map[string]any{"image": "example/dns:1", "environment": map[string]any{"PUBLIC": "public"}}}},
		"customComposeConfigStringWo":        `{"services":{"dns":{"environment":{"PASSWORD":"fixture-overlay-password"}}}}`,
		"customComposeConfigStringWoVersion": 1,
	})
	if !structured {
		news["customComposeConfigString"] = resource.NewStringProperty(`{"services":{"dns":{"image":"example/dns:1","environment":{"PUBLIC":"public"}}}}`)
		delete(news, "compose")
	}
	missingVersion := cloneProperties(t, news)
	delete(missingVersion, "customComposeConfigStringWoVersion")
	invalid, err := p.Check(ctx, plugin.CheckRequest{URN: urn, News: missingVersion})
	require.NoError(t, err)
	require.NotEmpty(t, invalid.Failures, "overlay requires a persisted version trigger")
	checked, err := p.Check(ctx, plugin.CheckRequest{URN: urn, News: news})
	require.NoError(t, err)
	require.Empty(t, checked.Failures)
	created, err := p.Create(ctx, plugin.CreateRequest{URN: urn, Properties: checked.Properties})
	require.NoError(t, err)
	require.False(t, created.Properties["customComposeConfigStringWo"].HasValue())
	nas.mu.Lock()
	require.Equal(t, "fixture-overlay-password", nas.document["services"].(map[string]any)["dns"].(map[string]any)["environment"].(map[string]any)["PASSWORD"])
	nas.mu.Unlock()
	read, err := p.Read(ctx, plugin.ReadRequest{URN: urn, ID: created.ID, Inputs: cloneProperties(t, checked.Properties), State: created.Properties})
	require.NoError(t, err)
	assertProjected := func(outputs resource.PropertyMap) {
		t.Helper()
		require.False(t, outputs["customComposeConfigStringWo"].HasValue())
		if structured {
			env := outputs["compose"].ObjectValue()["services"].ObjectValue()["dns"].ObjectValue()["environment"].ObjectValue()
			require.False(t, env["PASSWORD"].HasValue(), "overlay must not return through structured read-back")
		} else {
			require.NotContains(t, unwrap(outputs["customComposeConfigString"]).StringValue(), "PASSWORD")
		}
	}
	assertProjected(read.Outputs)
	diff, err := p.Diff(ctx, plugin.DiffRequest{URN: urn, ID: created.ID, OldInputs: read.Inputs, OldOutputs: read.Outputs, NewInputs: checked.Properties})
	require.NoError(t, err)
	require.Equal(t, plugin.DiffNone, diff.Changes)
	nas.mu.Lock()
	require.Equal(t, 1, nas.reads, "read-back must use a single upstream app.config call")
	nas.document["services"].(map[string]any)["dns"].(map[string]any)["image"] = "example/dns:2"
	nas.mu.Unlock()
	drift, err := p.Read(ctx, plugin.ReadRequest{URN: urn, ID: created.ID, Inputs: cloneProperties(t, read.Inputs), State: read.Outputs})
	require.NoError(t, err)
	assertProjected(drift.Outputs)
	diff, err = p.Diff(ctx, plugin.DiffRequest{URN: urn, ID: created.ID, OldInputs: drift.Inputs, OldOutputs: drift.Outputs, NewInputs: checked.Properties})
	require.NoError(t, err)
	require.Equal(t, plugin.DiffSome, diff.Changes)
	require.Empty(t, diff.ReplaceKeys)
	corrected, err := p.Update(ctx, plugin.UpdateRequest{URN: urn, ID: created.ID, OldInputs: drift.Inputs, OldOutputs: drift.Outputs, NewInputs: checked.Properties})
	require.NoError(t, err)
	rotated := cloneProperties(t, checked.Properties)
	rotated["customComposeConfigStringWo"] = resource.MakeSecret(resource.NewStringProperty(`{"services":{"dns":{"environment":{"PASSWORD":"fixture-rotated"}}}}`))
	rotated["customComposeConfigStringWoVersion"] = resource.NewNumberProperty(2)
	// Rotation must deploy even when the public base has not changed.
	diff, err = p.Diff(ctx, plugin.DiffRequest{URN: urn, ID: created.ID, OldInputs: checked.Properties, OldOutputs: corrected.Properties, NewInputs: rotated})
	require.NoError(t, err)
	require.Equal(t, plugin.DiffSome, diff.Changes)
	require.Empty(t, diff.ReplaceKeys)
	updated, err := p.Update(ctx, plugin.UpdateRequest{URN: urn, ID: created.ID, OldInputs: checked.Properties, OldOutputs: corrected.Properties, NewInputs: rotated})
	require.NoError(t, err)
	require.False(t, updated.Properties["customComposeConfigStringWo"].HasValue())
	nas.mu.Lock()
	require.Equal(t, "fixture-rotated", nas.document["services"].(map[string]any)["dns"].(map[string]any)["environment"].(map[string]any)["PASSWORD"])
	require.Equal(t, "example/dns:1", nas.document["services"].(map[string]any)["dns"].(map[string]any)["image"])
	require.Equal(t, 3, nas.writes, "only creation, drift correction, and rotation may write")
	nas.mu.Unlock()
}
