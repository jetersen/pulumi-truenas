# Provider development

- Keep this provider a thin bridge over `truenas/terraform-provider-truenas`.
- Change mappings in `provider/resources.go`; regenerate schema and SDKs instead of editing generated files.
- Run `make provider sdk test` and compile all supported SDKs before releasing.
- Publish NuGet and npm only to GitHub Packages. Publish Python distributions and provider archives only to GitHub Releases. Do not publish to nuget.org, npmjs.org, or PyPI.
- Keep credentials, live NAS configuration, and state out of this repository. Unit tests must not connect to a live NAS.
- Use conventional commits.
