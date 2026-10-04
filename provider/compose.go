package truenas

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"go.yaml.in/yaml/v3"
)

// These are containers for arbitrary application data, not a guess based on
// names like PASSWORD. Additional application-specific locations are explicit.
var composeSecretPaths = []string{
	"/services/*/environment/*", "/services/*/command", "/services/*/entrypoint",
	"/services/*/labels/*", "/services/*/annotations/*", "/services/*/healthcheck/test",
	"/services/*/build/args/*", "/services/*/build/labels/*",
	"/services/*/logging/options/*", "/services/*/storage_opt/*",
	"/configs/*/content", "/secrets", "/volumes/*/driver_opts/*", "/networks/*/driver_opts/*",
}

func composeCheck(ctx context.Context, props, _ resource.PropertyMap) (resource.PropertyMap, error) {
	c, raw := props["compose"], props["customComposeConfigString"]
	if c.HasValue() && raw.HasValue() {
		return nil, errors.New("compose and customComposeConfigString are mutually exclusive")
	}
	if c.HasValue() {
		custom := unwrap(props["customApp"])
		if custom.IsNull() || (custom.IsBool() && !custom.BoolValue()) {
			return nil, errors.New("compose requires customApp=true")
		}
		value := unwrap(c)
		if !value.IsComputed() && !value.IsOutput() {
			if !value.IsObject() {
				return nil, errors.New("compose must be an object")
			}
			services := unwrap(value.ObjectValue()["services"])
			if !services.IsObject() && !services.IsComputed() && !services.IsOutput() {
				return nil, errors.New("compose.services must be an object")
			}
		}
	}
	return composeProperties(ctx, props)
}

func composeProperties(_ context.Context, props resource.PropertyMap) (resource.PropertyMap, error) {
	// Deep copy: transforms must not mutate Check's original inputs or old state.
	result := props.Copy()
	if raw := unwrap(result["customComposeConfigString"]); raw.IsString() {
		canonical, err := canonicalCompose(raw.StringValue())
		if err != nil {
			return nil, err
		}
		result["customComposeConfigString"] = resource.MakeSecret(resource.NewStringProperty(canonical))
	}
	c, ok := result["compose"]
	if !ok || c.IsNull() {
		return result, nil
	}
	paths := append([]string{}, composeSecretPaths...)
	if extra, ok := result["composeSensitivePaths"]; ok && !extra.IsNull() {
		extra = unwrap(extra)
		if extra.ContainsUnknowns() {
			if !c.IsSecret() {
				c = resource.MakeSecret(c)
			}
			result["compose"] = c
			return result, nil
		}
		if !extra.IsArray() {
			return nil, errors.New("composeSensitivePaths must be an array of JSON pointers")
		}
		for _, path := range extra.ArrayValue() {
			path = unwrap(path)
			if !path.IsString() {
				return nil, errors.New("composeSensitivePaths must contain JSON pointers")
			}
			if _, err := pointerParts(path.StringValue()); err != nil {
				return nil, err
			}
			paths = append(paths, path.StringValue())
		}
	}
	for _, path := range paths {
		parts, _ := pointerParts(path)
		c = secretAt(c, parts)
	}
	// Compose extension fields are arbitrary and may contain credentials. Protect
	// their full value wherever they occur, including future extension fields.
	c = secretExtensions(c)
	result["compose"] = c
	return result, nil
}

func canonicalCompose(s string) (string, error) {
	bad := errors.New("invalid Compose YAML: expected one object document with services; document contents are omitted to protect secrets")
	decoder := yaml.NewDecoder(strings.NewReader(s))
	var value map[string]any
	if err := decoder.Decode(&value); err != nil || value == nil {
		return "", bad
	}
	if _, ok := value["services"].(map[string]any); !ok {
		return "", bad
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return "", bad
	}
	b, err := json.Marshal(value)
	if err != nil {
		return "", bad
	}
	// Compact JSON is deterministic, valid YAML, and preserves scalar types.
	return string(b), nil
}

func unwrap(v resource.PropertyValue) resource.PropertyValue {
	for v.IsSecret() {
		v = v.SecretValue().Element
	}
	return v
}

func pointerParts(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	} // JSON pointer for the whole document.
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New("composeSensitivePaths entries must be JSON pointers starting with /, or empty for the whole document")
	}
	parts := strings.Split(path[1:], "/")
	for i, part := range parts {
		for j := 0; j < len(part); j++ {
			if part[j] == '~' {
				if j+1 == len(part) || (part[j+1] != '0' && part[j+1] != '1') {
					return nil, errors.New("invalid JSON pointer escape in composeSensitivePaths")
				}
				j++
			}
		}
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

func secretAt(v resource.PropertyValue, parts []string) resource.PropertyValue {
	if v.IsNull() || v.IsSecret() {
		return v
	}
	if len(parts) == 0 {
		return resource.MakeSecret(v)
	}
	if v.IsComputed() || v.IsOutput() {
		return resource.MakeSecret(v)
	}
	key := parts[0]
	if len(parts) == 1 && key == "*" && !v.IsObject() {
		return resource.MakeSecret(v)
	}
	if v.IsObject() {
		obj := v.ObjectValue().Copy()
		for k, child := range obj {
			if key == "*" || string(k) == key {
				obj[k] = secretAt(child, parts[1:])
			}
		}
		return resource.NewObjectProperty(obj)
	}
	if v.IsArray() {
		// Array elements can move during refresh. Mask the entire array rather than
		// allowing an index change to expose a previously secret value.
		if len(v.ArrayValue()) > 0 {
			return resource.MakeSecret(v)
		}
	}
	return v
}

func secretExtensions(v resource.PropertyValue) resource.PropertyValue {
	if v.IsSecret() {
		return v
	}
	if v.IsObject() {
		obj := v.ObjectValue().Copy()
		for k, child := range obj {
			if strings.HasPrefix(string(k), "x-") && !child.IsSecret() {
				obj[k] = resource.MakeSecret(child)
			} else {
				obj[k] = secretExtensions(child)
			}
		}
		return resource.NewObjectProperty(obj)
	}
	if v.IsArray() {
		arr := append([]resource.PropertyValue{}, v.ArrayValue()...)
		for i := range arr {
			arr[i] = secretExtensions(arr[i])
		}
		return resource.NewArrayProperty(arr)
	}
	return v
}
