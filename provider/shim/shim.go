package shim

import (
	"github.com/hashicorp/terraform-plugin-framework/provider"
	upstream "github.com/truenas/terraform-provider-truenas/internal/provider"
)

func NewProvider() provider.Provider { return upstream.New("1.5.4")() }
