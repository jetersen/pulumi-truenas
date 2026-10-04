package main

import (
	"context"
	_ "embed"

	truenas "github.com/jetersen/pulumi-truenas/provider"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/pf/tfbridge"
)

//go:embed schema.json
var schema []byte

func main() {
	tfbridge.Main(context.Background(), "truenas", truenas.Provider(), tfbridge.ProviderMetadata{PackageSchema: schema})
}
