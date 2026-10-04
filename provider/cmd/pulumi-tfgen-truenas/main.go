package main

import (
	truenas "github.com/jetersen/pulumi-truenas/provider"
	"github.com/pulumi/pulumi-terraform-bridge/v3/pkg/pf/tfgen"
)

func main() { tfgen.Main("truenas", truenas.Provider()) }
