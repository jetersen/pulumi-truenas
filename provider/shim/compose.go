package shim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"math/big"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/truenas/terraform-provider-truenas/internal/client"
)

// Keep deployment, identity, and lifecycle behavior in the upstream resource.
// Only adapt the structured Compose input and add read-back via app.config.
type appDelegate interface {
	resource.Resource
	resource.ResourceWithConfigure
	resource.ResourceWithImportState
	resource.ResourceWithIdentity
}

type composeApp struct {
	appDelegate
	readConfig func(context.Context, string) (json.RawMessage, error)
}

func (r *composeApp) upstreamSchema(ctx context.Context) schema.Schema {
	var resp resource.SchemaResponse
	r.appDelegate.Schema(ctx, resource.SchemaRequest{}, &resp)
	return resp.Schema
}

func (r *composeApp) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	r.appDelegate.Schema(ctx, req, resp)
	resp.Schema.Attributes["compose"] = schema.DynamicAttribute{
		Optional:    true,
		Description: "Structured Docker Compose document for a custom app. Refreshed from app.config. Mutually exclusive with customComposeConfigString. Environment values and other potentially sensitive fields are secret by default.",
	}
	resp.Schema.Attributes["compose_sensitive_paths"] = schema.ListAttribute{
		Optional: true, ElementType: types.StringType,
		Description: "Additional secret fields in compose, expressed as JSON pointers (for example /services/dns/command). A whole path segment of * matches every object key or array element. Defaults cannot be disabled.",
	}
	a := resp.Schema.Attributes["custom_compose_config_string"].(schema.StringAttribute)
	a.Description = "Docker Compose YAML for custom apps. Refreshed from app.config; the entire document is secret. Use compose for field-level diffs."
	resp.Schema.Attributes["custom_compose_config_string"] = a
}

func (r *composeApp) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.appDelegate.Configure(ctx, req, resp)
	if c, ok := req.ProviderData.(*client.Client); ok {
		r.readConfig = func(ctx context.Context, name string) (json.RawMessage, error) {
			return c.CallRead(ctx, "app.config", name)
		}
	}
}

// translate removes adapter-only attributes before passing values to the
// upstream model. JSON is valid YAML, and gives deterministic serialization.
func (r *composeApp) translate(ctx context.Context, raw tftypes.Value) (tftypes.Value, error) {
	s := r.upstreamSchema(ctx)
	if raw.IsNull() {
		return tftypes.NewValue(s.Type().TerraformType(ctx), nil), nil
	}
	var attrs map[string]tftypes.Value
	if err := raw.As(&attrs); err != nil {
		return tftypes.Value{}, err
	}
	attrs = maps.Clone(attrs)
	if c, ok := attrs["compose"]; ok && !c.IsNull() {
		if !c.IsFullyKnown() {
			attrs["custom_compose_config_string"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
		} else {
			v, err := valueToJSON(c)
			if err != nil {
				return tftypes.Value{}, err
			}
			b, err := json.Marshal(v)
			if err != nil {
				return tftypes.Value{}, err
			}
			attrs["custom_compose_config_string"] = tftypes.NewValue(tftypes.String, string(b))
		}
	}
	delete(attrs, "compose")
	delete(attrs, "compose_sensitive_paths")
	return tftypes.NewValue(s.Type().TerraformType(ctx), attrs), nil
}

// extend restores adapter inputs after an upstream operation. Never duplicate
// the serialized document in state when the structured input was used.
func (r *composeApp) extend(ctx context.Context, state tfsdk.State, original tftypes.Value) tfsdk.State {
	var sr resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &sr)
	if state.Raw.IsNull() {
		return tfsdk.State{Schema: sr.Schema, Raw: tftypes.NewValue(sr.Schema.Type().TerraformType(ctx), nil)}
	}
	var attrs, extra map[string]tftypes.Value
	_ = state.Raw.As(&attrs)
	attrs = maps.Clone(attrs)
	_ = original.As(&extra)
	for _, key := range []string{"compose", "compose_sensitive_paths"} {
		attrs[key] = tftypes.NewValue(sr.Schema.Attributes[key].GetType().TerraformType(ctx), nil)
		if v, ok := extra[key]; ok {
			attrs[key] = v
		}
	}
	if !attrs["compose"].IsNull() {
		attrs["custom_compose_config_string"] = tftypes.NewValue(tftypes.String, nil)
	}
	return tfsdk.State{Schema: sr.Schema, Raw: tftypes.NewValue(sr.Schema.Type().TerraformType(ctx), attrs)}
}

func adapterError(ds *diag.Diagnostics) {
	ds.AddError("Invalid Compose configuration", "Could not translate the Compose document. Check the document structure; values are omitted to protect secrets.")
}

func (r *composeApp) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	original := req.Plan.Raw
	raw, err := r.translate(ctx, original)
	if err != nil {
		adapterError(&resp.Diagnostics)
		return
	}
	s := r.upstreamSchema(ctx)
	req.Plan = tfsdk.Plan{Schema: s, Raw: raw}
	config, err := r.translate(ctx, req.Config.Raw)
	if err != nil {
		adapterError(&resp.Diagnostics)
		return
	}
	req.Config = tfsdk.Config{Schema: s, Raw: config}
	resp.State = tfsdk.State{Schema: s, Raw: raw}
	r.appDelegate.Create(ctx, req, resp)
	resp.Diagnostics = protectDiagnostics(resp.Diagnostics)
	resp.State = r.extend(ctx, resp.State, original)
}

func (r *composeApp) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	original := req.Plan.Raw
	plan, err := r.translate(ctx, original)
	if err != nil {
		adapterError(&resp.Diagnostics)
		return
	}
	state, err := r.translate(ctx, req.State.Raw)
	if err != nil {
		adapterError(&resp.Diagnostics)
		return
	}
	config, err := r.translate(ctx, req.Config.Raw)
	if err != nil {
		adapterError(&resp.Diagnostics)
		return
	}
	s := r.upstreamSchema(ctx)
	req.Plan = tfsdk.Plan{Schema: s, Raw: plan}
	req.State = tfsdk.State{Schema: s, Raw: state}
	req.Config = tfsdk.Config{Schema: s, Raw: config}
	resp.State = req.State
	r.appDelegate.Update(ctx, req, resp)
	resp.Diagnostics = protectDiagnostics(resp.Diagnostics)
	resp.State = r.extend(ctx, resp.State, original)
}

func (r *composeApp) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	original := req.State.Raw
	raw, err := r.translate(ctx, original)
	if err != nil {
		adapterError(&resp.Diagnostics)
		return
	}
	req.State = tfsdk.State{Schema: r.upstreamSchema(ctx), Raw: raw}
	resp.State = req.State
	r.appDelegate.Delete(ctx, req, resp)
	resp.Diagnostics = protectDiagnostics(resp.Diagnostics)
	resp.State = r.extend(ctx, resp.State, original)
}

func (r *composeApp) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	original := req.State.Raw
	raw, err := r.translate(ctx, original)
	if err != nil {
		adapterError(&resp.Diagnostics)
		return
	}
	req.State = tfsdk.State{Schema: r.upstreamSchema(ctx), Raw: raw}
	resp.State = req.State
	r.appDelegate.Read(ctx, req, resp)
	resp.Diagnostics = protectDiagnostics(resp.Diagnostics)
	resp.State = r.extend(ctx, resp.State, original)
	if resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
		return
	}
	var attrs map[string]tftypes.Value
	_ = resp.State.Raw.As(&attrs)
	var custom bool
	_ = attrs["custom_app"].As(&custom)
	if !custom {
		return
	}
	var name string
	_ = attrs["name"].As(&name)
	if r.readConfig == nil {
		resp.Diagnostics.AddError("Compose read-back unavailable", "The TrueNAS client was not configured.")
		return
	}
	b, err := r.readConfig(ctx, name)
	if err != nil {
		resp.Diagnostics.AddError("Compose read-back failed", "Could not read app.config. Refresh cannot verify the Compose configuration; API details are omitted to protect secrets.")
		return
	}
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	err = decoder.Decode(&document)
	_, servicesOK := document["services"].(map[string]any)
	if err != nil || document == nil || !servicesOK {
		resp.Diagnostics.AddError("Invalid Compose read-back", "app.config did not return a Compose object with services. The previous state has been retained.")
		return
	}
	// Imports use the legacy secret string until the program explicitly selects
	// compose. The bridge cannot currently extract schema-free dynamic objects
	// during import (nested objects cause extractSchemaInputs to panic).
	if !attrs["compose"].IsNull() {
		attrs["compose"] = jsonToValue(document)
		attrs["custom_compose_config_string"] = tftypes.NewValue(tftypes.String, nil)
	} else {
		b, _ = json.Marshal(document)
		attrs["custom_compose_config_string"] = tftypes.NewValue(tftypes.String, string(b))
	}
	resp.State.Raw = tftypes.NewValue(resp.State.Schema.Type().TerraformType(ctx), attrs)
}

// Dynamic Terraform values retain the concrete nested object/tuple types.
func valueToJSON(v tftypes.Value) (any, error) {
	if v.IsNull() {
		return nil, nil
	}
	if !v.IsKnown() {
		return nil, errors.New("unknown value")
	}
	switch t := v.Type(); {
	case t.Is(tftypes.String):
		var x string
		err := v.As(&x)
		return x, err
	case t.Is(tftypes.Bool):
		var x bool
		err := v.As(&x)
		return x, err
	case t.Is(tftypes.Number):
		var x big.Float
		if err := v.As(&x); err != nil {
			return nil, err
		}
		return json.Number(x.Text('g', -1)), nil
	default:
		switch t.(type) {
		case tftypes.Object, tftypes.Map:
			var x map[string]tftypes.Value
			if err := v.As(&x); err != nil {
				return nil, err
			}
			result := map[string]any{}
			for k, child := range x {
				y, err := valueToJSON(child)
				if err != nil {
					return nil, err
				}
				result[k] = y
			}
			return result, nil
		case tftypes.List, tftypes.Tuple, tftypes.Set:
			var x []tftypes.Value
			if err := v.As(&x); err != nil {
				return nil, err
			}
			result := make([]any, len(x))
			for i, child := range x {
				y, err := valueToJSON(child)
				if err != nil {
					return nil, err
				}
				result[i] = y
			}
			return result, nil
		}
	}
	return nil, errors.New("unsupported value type")
}

func jsonToValue(x any) tftypes.Value {
	switch x := x.(type) {
	case nil:
		return tftypes.NewValue(tftypes.DynamicPseudoType, nil)
	case string:
		return tftypes.NewValue(tftypes.String, x)
	case bool:
		return tftypes.NewValue(tftypes.Bool, x)
	case json.Number:
		n, _, err := big.ParseFloat(string(x), 10, 512, big.ToNearestEven)
		if err != nil {
			panic("invalid JSON number")
		}
		return tftypes.NewValue(tftypes.Number, n)
	case float64:
		return tftypes.NewValue(tftypes.Number, x)
	case map[string]any:
		vals, ts := map[string]tftypes.Value{}, map[string]tftypes.Type{}
		for k, v := range x {
			vals[k] = jsonToValue(v)
			ts[k] = vals[k].Type()
		}
		return tftypes.NewValue(tftypes.Object{AttributeTypes: ts}, vals)
	case []any:
		vals, ts := make([]tftypes.Value, len(x)), make([]tftypes.Type, len(x))
		for i, v := range x {
			vals[i] = jsonToValue(v)
			ts[i] = vals[i].Type()
		}
		return tftypes.NewValue(tftypes.Tuple{ElementTypes: ts}, vals)
	default:
		panic("jsonToValue requires JSON-decoded data")
	}
}

// API error details may echo the submitted Compose document. Never forward
// those details into Pulumi diagnostics, including for the legacy secret field.
func protectDiagnostics(input diag.Diagnostics) diag.Diagnostics {
	var output diag.Diagnostics
	for _, d := range input {
		detail := "TrueNAS app operation failed. API details are omitted because they may contain Compose secrets. Check the NAS logs for details."
		if d.Severity() == diag.SeverityError {
			output.AddError(d.Summary(), detail)
		} else {
			output.AddWarning(d.Summary(), detail)
		}
	}
	return output
}
