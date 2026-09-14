package agentsdk

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	"github.com/airlockrun/goai/schema"
)

// agentTypeSchema refines generated schemas without moving their reference root.
// Separate constraints preserve goai metadata, nullability and local $defs while
// keeping fixed lengths and numeric widths in the task's immutable contract.
func agentTypeSchema(t reflect.Type) json.RawMessage {
	raw := schema.MustFromType(reflect.Zero(t).Interface()).MustJSON()
	var constraints func(reflect.Type) map[string]any
	constraints = func(t reflect.Type) map[string]any {
		c := map[string]any{}
		switch t.Kind() {
		case reflect.Pointer:
			if child := constraints(t.Elem()); len(child) > 0 {
				c["anyOf"] = []any{map[string]any{"type": "null"}, child}
			}
		case reflect.Struct:
			properties := map[string]any{}
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				if !field.IsExported() || field.Tag.Get("json") == "-" {
					continue
				}
				name := strings.Split(field.Tag.Get("json"), ",")[0]
				if name == "" {
					name = field.Name
				}
				if child := constraints(field.Type); len(child) > 0 {
					properties[name] = child
				}
			}
			if len(properties) > 0 {
				c["properties"] = properties
			}
		case reflect.Array, reflect.Slice:
			if t.Kind() == reflect.Array {
				c["minItems"], c["maxItems"] = t.Len(), t.Len()
			}
			if child := constraints(t.Elem()); len(child) > 0 {
				c["items"] = child
			}
		default:
			if min, max := agentNumericBounds(t); min != "" {
				c["minimum"], c["maximum"] = min, max
			}
		}
		return c
	}
	c := constraints(t)
	if len(c) == 0 {
		return raw
	}
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		panic(err)
	}
	allOf, _ := root["allOf"].([]any)
	root["allOf"] = append(allOf, c)
	raw, err := json.Marshal(root)
	if err != nil {
		panic(err)
	}
	return raw
}

// Bounds are JSON numbers, not float64s: rounding MaxInt64 or MaxUint64 would
// advertise values that encoding/json cannot decode into the declared Go type.
func agentNumericBounds(t reflect.Type) (json.Number, json.Number) {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		max := int64(uint64(1)<<(t.Bits()-1) - 1)
		return json.Number(strconv.FormatInt(-max-1, 10)), json.Number(strconv.FormatInt(max, 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		max := uint64(math.MaxUint64) >> (64 - t.Bits())
		return "0", json.Number(strconv.FormatUint(max, 10))
	case reflect.Float32, reflect.Float64:
		max := math.MaxFloat64
		if t.Kind() == reflect.Float32 {
			// Float32's shortest JSON encoding rounds outward at MaxFloat32.
			// Include that encoding so every finite native input remains valid.
			bound := strconv.FormatFloat(math.MaxFloat32, 'g', -1, 32)
			return json.Number("-" + bound), json.Number(bound)
		}
		integer, _ := new(big.Float).SetFloat64(max).Int(nil)
		return json.Number("-" + integer.String()), json.Number(integer.String())
	default:
		return "", ""
	}
}

// Check constraints that Go decoding can discard (array elements, null values)
// or reject only after partial decoding (numeric overflow). The wire value is
// inspected before allocating or populating the caller's typed result.
func validateAgentTypeValue(t reflect.Type, raw json.RawMessage) error {
	if !json.Valid(raw) {
		return errors.New("invalid JSON")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	var validate func(reflect.Type, any) error
	validate = func(t reflect.Type, value any) error {
		if t.Kind() == reflect.Pointer {
			if value == nil {
				return nil
			}
			return validate(t.Elem(), value)
		}
		if value == nil {
			return fmt.Errorf("null is not valid for %s", t)
		}
		switch t.Kind() {
		case reflect.Struct:
			object, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("expected object for %s", t)
			}
			fields := map[string]reflect.Type{}
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				if !field.IsExported() || field.Tag.Get("json") == "-" {
					continue
				}
				name := strings.Split(field.Tag.Get("json"), ",")[0]
				if name == "" {
					name = field.Name
				}
				fields[name] = field.Type
			}
			for name, child := range object {
				field, ok := fields[name]
				if !ok {
					return fmt.Errorf("unknown property %q", name)
				}
				if err := validate(field, child); err != nil {
					return fmt.Errorf("property %q: %w", name, err)
				}
			}
		case reflect.Array, reflect.Slice:
			items, ok := value.([]any)
			if !ok {
				return fmt.Errorf("expected array for %s", t)
			}
			if t.Kind() == reflect.Array && len(items) != t.Len() {
				return fmt.Errorf("%s requires exactly %d items, got %d", t, t.Len(), len(items))
			}
			for i, child := range items {
				if err := validate(t.Elem(), child); err != nil {
					return fmt.Errorf("item %d: %w", i, err)
				}
			}
		default:
			min, max := agentNumericBounds(t)
			if min == "" {
				return nil
			}
			number, ok := value.(json.Number)
			if !ok {
				return fmt.Errorf("expected number for %s", t)
			}
			n, ok := new(big.Rat).SetString(string(number))
			if !ok {
				return errors.New("invalid JSON number")
			}
			lo, _ := new(big.Rat).SetString(string(min))
			hi, _ := new(big.Rat).SetString(string(max))
			if n.Cmp(lo) < 0 || n.Cmp(hi) > 0 {
				return fmt.Errorf("number outside %s range [%s, %s]", t, min, max)
			}
			if t.Kind() != reflect.Float32 && t.Kind() != reflect.Float64 && !n.IsInt() {
				return fmt.Errorf("expected integer for %s", t)
			}
		}
		return nil
	}
	return validate(t, value)
}
