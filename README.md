# Pulumi TrueNAS

A community Pulumi provider bridged from the official
[TrueNAS Terraform provider](https://github.com/truenas/terraform-provider-truenas).
Supports C#, TypeScript/JavaScript, Python, and Go. This project is not affiliated
with TrueNAS or Pulumi.

The bridge uses the upstream JSON-RPC WebSocket client. Use a trusted HTTPS
hostname and an endpoint such as `wss://nas.example.com/api/current`. TrueNAS
25.10 and 26.0 are supported upstream; individual resources can require newer
TrueNAS versions. API credentials must be Pulumi secrets.

## Distribution

- NuGet: `Jetersen.Pulumi.TrueNas` at `https://nuget.pkg.github.com/jetersen/index.json`.
- npm: `@jetersen/pulumi-truenas` at `https://npm.pkg.github.com`.
- Python: `jetersen_pulumi_truenas` wheels attached to GitHub Releases.
- Go: `github.com/jetersen/pulumi-truenas/sdk/go/truenas`.
- Provider binaries, checksums, and SDK archives: GitHub Releases.

Nothing is published to nuget.org, npmjs.org, or PyPI. GitHub Packages requires
authentication even for public NuGet/npm packages. Use a classic token with
`read:packages` outside GitHub Actions. Release assets provide the same NuGet
and npm packages for installations without registry credentials.

Pin the SDK version and use package source mapping for `Jetersen.Pulumi.TrueNas`
so NuGet resolves it only from your chosen GitHub feed or downloaded local feed.
The generated SDK downloads the matching provider from this repository's releases.

```csharp
using Pulumi;
using TrueNas = Jetersen.Pulumi.TrueNas;

return await Deployment.RunAsync(() =>
{
    var config = new Config();
    var nas = new TrueNas.Provider("nas", new TrueNas.ProviderArgs
    {
        Endpoint = config.Require("endpoint"),
        ApiKey = config.RequireSecret("apiKey"),
    });
    _ = new TrueNas.App("dns", new TrueNas.AppArgs
    {
        Name = "dns",
        CustomApp = true,
        CustomComposeConfigString = File.ReadAllText("compose.yaml"),
    }, new CustomResourceOptions { Provider = nas, Protect = true });
});
```

Use the existing app name as the import ID when adopting an application.
Review a preview before applying. Deleting resources can delete NAS data;
protect resources containing data. Both Compose representations read live configuration
through `app.config` during refresh. Catalog app `values` and the legacy
`customComposeConfigString` remain entirely secret. C# exposes certificate content
as `CertificatePem` to avoid a class/property name collision.

## Compose diffs

Use the optional `compose` object instead of `customComposeConfigString` for
field-level diffs. The adapter serializes this object for the upstream app
resource; the app name, resource identity, and deployment API stay the same.
Keep image references in `compose.yaml` and parse that file into `compose` in the
Pulumi program. Renovate continues to use its built-in Docker Compose manager;
no custom regex or duplicate image version in application code is needed.
For example, in TypeScript with the `yaml` package installed:

```typescript
import { readFileSync } from "node:fs";
import { parse } from "yaml";

const compose = parse(readFileSync("compose.yaml", "utf8"));
compose.services.dns.environment.DNS_SERVER_ADMIN_PASSWORD = config.requireSecret("dnsPassword");

const app = new truenas.App("dns", {
    name: "dns",
    customApp: true,
    compose,
    composeSensitivePaths: ["/services/dns/labels/private-note"],
}, { provider: nas, protect: true });
```

C# programs can deserialize the same file to dictionaries and lists before
assigning `AppArgs.Compose`. Keep scalar types intact. Inject credentials through
Pulumi secret values rather than storing them in the Compose file.

Run `pulumi preview --refresh --diff` to compare live configuration with the
program. Standard Pulumi secret values remain secret on read-back. Other fields,
including ordinary environment variables, labels, and commands, stay visible.
The provider does not guess which application values are credentials.

Use `composeSensitivePaths` for sensitive locations that must be protected even
when discovered during refresh. Paths are JSON pointers; a complete `*` segment
matches all keys or elements, `~1` escapes `/`, and `~0` escapes `~`. An empty
pointer protects the entire document. No paths are selected automatically.
Arrays selected by a sensitive path are encrypted as a whole because elements
can move. New fields are visible unless protected by a secret marker or an
explicit path, so configure paths before reading externally managed credentials.

Read failures stop refresh rather than silently retaining stale configuration.
Avoid verbose provider/debug logs when working with secrets. The adapter supplies
read-back until it is available upstream; see
[terraform-provider-truenas#34](https://github.com/truenas/terraform-provider-truenas/issues/34).

Import an existing app by name first. Imports initially populate the fully secret
`customComposeConfigString`, working around the bridge's dynamic-object import
limitation. Then replace that input with an equivalent `compose` object. This is
an in-place state transition; equivalent documents do not trigger an app update.
Do not set both inputs. Object key order and YAML formatting do not cause drift;
array order and scalar types remain significant.

## Development and releases

Use Go 1.26.6+, Pulumi CLI 3.267.0+, Node.js 24, Python 3.13+, .NET 10, and a
current `pulumi-language-dotnet` plugin. The Plugin Framework shim exposes the
upstream internal constructor without maintaining an upstream fork. A focused app
adapter adds structured Compose configuration and read-back while delegating
deployment and identity to upstream. Compose tests use a local mock API, never a
live NAS. The CLI tests exercise YAML and C# programs and require their language hosts
bundled with Pulumi.

```sh
make provider sdk test
make test-compose-preview
dotnet build sdk/dotnet
cd sdk/nodejs && npm install && npm run build
```

Commit regenerated schema, bridge metadata, and the Go SDK. Other SDKs are
generated by CI and packaged as release assets. A `vX.Y.Z` tag publishes a release
and NuGet/npm packages using the repository's `GITHUB_TOKEN`. The release also
tags the Go SDK module. Provider builds cover Linux, macOS, and Windows on amd64
and arm64. Generated APIs reflect upstream coverage; release builds do not prove
live lifecycle compatibility for every resource.
