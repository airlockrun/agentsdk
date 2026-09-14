package agentruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

// validateJSON validates the declarative SDK schema vocabulary without coercing
// numbers or fetching remote references. Unsupported assertions fail closed.
func validateJSON(rawSchema, rawValue json.RawMessage) error {
	if err := checkSchema(rawSchema); err != nil {
		return err
	}
	var root, value any
	for _, item := range []struct {
		raw json.RawMessage
		dst *any
	}{{rawSchema, &root}, {rawValue, &value}} {
		if !json.Valid(item.raw) {
			return errors.New("invalid JSON schema or value")
		}
		d := json.NewDecoder(bytes.NewReader(item.raw))
		d.UseNumber()
		if err := d.Decode(item.dst); err != nil {
			return err
		}
	}
	var validate func(any, any, int) error
	validate = func(schema, value any, depth int) error {
		if depth > 128 {
			return errors.New("schema validation depth exceeded")
		}
		if allowed, ok := schema.(bool); ok {
			if !allowed {
				return errors.New("value is forbidden by schema")
			}
			return nil
		}
		s, ok := schema.(map[string]any)
		if !ok {
			return errors.New("schema must be an object or boolean")
		}
		for key := range s {
			switch key {
			case "$schema", "$id", "$defs", "definitions", "$comment", "title", "description", "default", "examples", "format", "readOnly", "writeOnly", "deprecated",
				"$ref", "type", "properties", "required", "additionalProperties", "items", "enum", "const", "anyOf", "oneOf", "allOf", "not",
				"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength", "pattern", "minItems", "maxItems", "uniqueItems", "minProperties", "maxProperties":
			default:
				return fmt.Errorf("unsupported schema keyword %q", key)
			}
		}
		if raw, exists := s["$ref"]; exists {
			ref, ok := raw.(string)
			if !ok || ref != "#" && !strings.HasPrefix(ref, "#/") {
				return errors.New("only local JSON pointer schema references are supported")
			}
			target := root
			if ref != "#" {
				for _, token := range strings.Split(ref[2:], "/") {
					object, ok := target.(map[string]any)
					if !ok {
						return fmt.Errorf("unresolved schema reference %q", ref)
					}
					target, ok = object[strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")]
					if !ok {
						return fmt.Errorf("unresolved schema reference %q", ref)
					}
				}
			}
			if err := validate(target, value, depth+1); err != nil {
				return err
			}
		}
		for _, key := range []string{"allOf", "anyOf", "oneOf"} {
			if raw, exists := s[key]; exists {
				branches, ok := raw.([]any)
				if !ok || len(branches) == 0 {
					return fmt.Errorf("invalid %s schema", key)
				}
				matches := 0
				for _, branch := range branches {
					if validate(branch, value, depth+1) == nil {
						matches++
					}
				}
				if key == "allOf" && matches != len(branches) || key == "anyOf" && matches == 0 || key == "oneOf" && matches != 1 {
					return fmt.Errorf("value does not match %s", key)
				}
			}
		}
		if negated, exists := s["not"]; exists && validate(negated, value, depth+1) == nil {
			return errors.New("value matches forbidden schema")
		}
		if c, exists := s["const"]; exists && !jsonEqual(c, value) {
			return errors.New("value does not match const")
		}
		if raw, exists := s["enum"]; exists {
			values, ok := raw.([]any)
			if !ok {
				return errors.New("invalid enum schema")
			}
			found := false
			for _, v := range values {
				found = found || jsonEqual(v, value)
			}
			if !found {
				return errors.New("value is not in enum")
			}
		}
		if raw, exists := s["type"]; exists {
			types, ok := raw.([]any)
			if !ok {
				types = []any{raw}
			}
			matches := false
			for _, typ := range types {
				switch typ {
				case "null":
					matches = matches || value == nil
				case "object":
					_, ok := value.(map[string]any)
					matches = matches || ok
				case "array":
					_, ok := value.([]any)
					matches = matches || ok
				case "string":
					_, ok := value.(string)
					matches = matches || ok
				case "boolean":
					_, ok := value.(bool)
					matches = matches || ok
				case "number", "integer":
					n, ok := value.(json.Number)
					if ok {
						r, valid := new(big.Rat).SetString(string(n))
						matches = matches || valid && (typ == "number" || r.IsInt())
					}
				default:
					return fmt.Errorf("invalid schema type %v", typ)
				}
			}
			if !matches {
				return fmt.Errorf("value does not match type %v", raw)
			}
		}
		checkSize := func(n int, minKey, maxKey string) error {
			for _, key := range []string{minKey, maxKey} {
				if raw, exists := s[key]; exists {
					bound, ok := raw.(json.Number)
					if !ok {
						return fmt.Errorf("invalid %s", key)
					}
					r, ok := new(big.Rat).SetString(string(bound))
					if !ok || !r.IsInt() || r.Sign() < 0 || !r.Num().IsInt64() {
						return fmt.Errorf("invalid %s", key)
					}
					i := r.Num().Int64()
					if key == minKey && int64(n) < i || key == maxKey && int64(n) > i {
						return fmt.Errorf("value violates %s", key)
					}
				}
			}
			return nil
		}
		switch v := value.(type) {
		case map[string]any:
			if err := checkSize(len(v), "minProperties", "maxProperties"); err != nil {
				return err
			}
			properties, _ := s["properties"].(map[string]any)
			if raw, exists := s["required"]; exists {
				required, ok := raw.([]any)
				if !ok {
					return errors.New("invalid required schema")
				}
				for _, rawKey := range required {
					key, ok := rawKey.(string)
					if !ok {
						return errors.New("invalid required property")
					}
					if _, ok := v[key]; !ok {
						return fmt.Errorf("required property %q is missing", key)
					}
				}
			}
			for key, child := range v {
				property, exists := properties[key]
				if !exists {
					property, exists = s["additionalProperties"]
				}
				if exists {
					if err := validate(property, child, depth+1); err != nil {
						return fmt.Errorf("property %q: %w", key, err)
					}
				}
			}
		case []any:
			if err := checkSize(len(v), "minItems", "maxItems"); err != nil {
				return err
			}
			for i, child := range v {
				if item, exists := s["items"]; exists {
					if err := validate(item, child, depth+1); err != nil {
						return fmt.Errorf("item %d: %w", i, err)
					}
				}
				if s["uniqueItems"] == true {
					for _, previous := range v[:i] {
						if jsonEqual(previous, child) {
							return errors.New("array items must be unique")
						}
					}
				}
			}
		case string:
			if err := checkSize(utf8.RuneCountInString(v), "minLength", "maxLength"); err != nil {
				return err
			}
			if raw, exists := s["pattern"]; exists {
				pattern, ok := raw.(string)
				if !ok {
					return errors.New("invalid pattern schema")
				}
				matched, err := regexp.MatchString(pattern, v)
				if err != nil {
					return err
				}
				if !matched {
					return errors.New("string does not match pattern")
				}
			}
		case json.Number:
			n, ok := new(big.Rat).SetString(string(v))
			if !ok {
				return errors.New("invalid JSON number")
			}
			for _, key := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"} {
				if raw, exists := s[key]; exists {
					bound, ok := raw.(json.Number)
					if !ok {
						return fmt.Errorf("invalid %s", key)
					}
					b, ok := new(big.Rat).SetString(string(bound))
					if !ok {
						return fmt.Errorf("invalid %s", key)
					}
					cmp := n.Cmp(b)
					if key == "minimum" && cmp < 0 || key == "maximum" && cmp > 0 || key == "exclusiveMinimum" && cmp <= 0 || key == "exclusiveMaximum" && cmp >= 0 {
						return fmt.Errorf("number violates %s", key)
					}
					if key == "multipleOf" && (b.Sign() <= 0 || !new(big.Rat).Quo(n, b).IsInt()) {
						return errors.New("number violates multipleOf")
					}
				}
			}
		}
		return nil
	}
	return validate(root, value, 0)
}

func jsonEqual(a, b any) bool {
	if x, ok := a.(json.Number); ok {
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		xr, xok := new(big.Rat).SetString(string(x))
		yr, yok := new(big.Rat).SetString(string(y))
		return xok && yok && xr.Cmp(yr) == 0
	}
	switch x := a.(type) {
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !jsonEqual(v, w) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

// Check every branch before offering a contract to the model, including schemas
// in optional properties and negations. Unsupported assertions cannot become a
// successful match by being hidden in a not/oneOf branch.
func checkSchema(raw json.RawMessage) error {
	if !json.Valid(raw) {
		return errors.New("invalid JSON schema")
	}
	var root any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&root); err != nil {
		return err
	}
	checkedRefs := map[string]bool{"#": true}
	var check func(any, int) error
	check = func(value any, depth int) error {
		if depth > 128 {
			return errors.New("schema depth exceeded")
		}
		if _, ok := value.(bool); ok {
			return nil
		}
		s, ok := value.(map[string]any)
		if !ok {
			return errors.New("schema must be an object or boolean")
		}
		for key, v := range s {
			switch key {
			case "$schema", "$comment", "title", "description", "format":
				if _, ok := v.(string); !ok {
					return fmt.Errorf("invalid %s", key)
				}
			case "default", "examples", "const":
			case "readOnly", "writeOnly", "deprecated", "uniqueItems":
				if _, ok := v.(bool); !ok {
					return fmt.Errorf("invalid %s", key)
				}
			case "properties", "$defs", "definitions":
				properties, ok := v.(map[string]any)
				if !ok {
					return fmt.Errorf("invalid %s", key)
				}
				for name, child := range properties {
					if err := check(child, depth+1); err != nil {
						return fmt.Errorf("%s %q: %w", key, name, err)
					}
				}
			case "items", "additionalProperties", "not":
				if err := check(v, depth+1); err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
			case "allOf", "anyOf", "oneOf":
				branches, ok := v.([]any)
				if !ok || len(branches) == 0 {
					return fmt.Errorf("invalid %s", key)
				}
				for _, branch := range branches {
					if err := check(branch, depth+1); err != nil {
						return err
					}
				}
			case "$ref":
				ref, ok := v.(string)
				if !ok || ref != "#" && !strings.HasPrefix(ref, "#/") {
					return errors.New("only local JSON pointer schema references are supported")
				}
				target := root
				if ref != "#" {
					for _, token := range strings.Split(ref[2:], "/") {
						object, ok := target.(map[string]any)
						if !ok {
							return fmt.Errorf("unresolved schema reference %q", ref)
						}
						target, ok = object[strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")]
						if !ok {
							return fmt.Errorf("unresolved schema reference %q", ref)
						}
					}
				}
				switch target.(type) {
				case map[string]any, bool:
				default:
					return fmt.Errorf("reference %q is not a schema", ref)
				}
				if !checkedRefs[ref] {
					checkedRefs[ref] = true
					if err := check(target, depth+1); err != nil {
						return err
					}
				}
			case "type", "required":
				values, ok := v.([]any)
				if !ok {
					if key == "required" {
						return errors.New("invalid required")
					}
					values = []any{v}
				}
				if key == "type" && len(values) == 0 {
					return errors.New("empty type union")
				}
				seen := map[string]bool{}
				for _, raw := range values {
					str, ok := raw.(string)
					if !ok || seen[str] {
						return fmt.Errorf("invalid %s", key)
					}
					seen[str] = true
					if key == "type" {
						switch str {
						case "null", "object", "array", "string", "boolean", "number", "integer":
						default:
							return fmt.Errorf("invalid type %q", str)
						}
					}
				}
			case "enum":
				values, ok := v.([]any)
				if !ok || len(values) == 0 {
					return errors.New("invalid enum")
				}
			case "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties":
				n, ok := v.(json.Number)
				if !ok {
					return fmt.Errorf("invalid %s", key)
				}
				r, ok := new(big.Rat).SetString(string(n))
				if !ok || key == "multipleOf" && r.Sign() <= 0 {
					return fmt.Errorf("invalid %s", key)
				}
				if strings.HasSuffix(key, "Length") || strings.HasSuffix(key, "Items") || strings.HasSuffix(key, "Properties") {
					if !r.IsInt() || r.Sign() < 0 || !r.Num().IsInt64() {
						return fmt.Errorf("invalid %s", key)
					}
				}
			case "pattern":
				pattern, ok := v.(string)
				if !ok {
					return errors.New("invalid pattern")
				}
				if _, err := regexp.Compile(pattern); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unsupported schema keyword %q", key)
			}
		}
		return nil
	}
	return check(root, 0)
}
