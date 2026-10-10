package shim

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	upstream "github.com/truenas/terraform-provider-truenas/internal/provider"
	"github.com/truenas/terraform-provider-truenas/internal/resources/app"
)

func NewProvider() provider.Provider {
	return &composeProvider{upstream.New("1.5.11")().(*upstream.TrueNASProvider)}
}

type composeProvider struct{ *upstream.TrueNASProvider }

func (p *composeProvider) Resources(ctx context.Context) []func() resource.Resource {
	factories := p.TrueNASProvider.Resources(ctx)
	for i, factory := range factories {
		factories[i] = func() resource.Resource {
			r := factory()
			if a, ok := r.(*app.AppResource); ok {
				return &composeApp{appDelegate: a}
			}
			return r
		}
	}
	return factories
}
