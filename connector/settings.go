package connector

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

// Secret marks a sensitive host-supplied setting. Its value is never included
// in connector manifests or logs by the SDK.
type Secret string

type settingsField struct {
	index        int
	name         string
	jsonName     string
	kind         string
	description  string
	required     bool
	defaultValue string
	enum         []string
}

func settingsSchema(settings any) ([]protocol.SettingDescriptor, []settingsField, error) {
	if settings == nil {
		return []protocol.SettingDescriptor{}, nil, nil
	}
	value := reflect.ValueOf(settings)
	if value.Kind() != reflect.Pointer || value.IsNil() || value.Elem().Kind() != reflect.Struct {
		return nil, nil, errors.New("connector: Config.Settings must be a non-nil pointer to a struct")
	}
	typeOf := value.Elem().Type()
	if implementsSettingsMarshaler(typeOf) || implementsSettingsMarshaler(reflect.PointerTo(typeOf)) {
		return nil, nil, errors.New("connector: settings struct cannot implement custom JSON or text marshaling")
	}
	descriptors := make([]protocol.SettingDescriptor, 0, typeOf.NumField())
	fields := make([]settingsField, 0, typeOf.NumField())
	seen := make(map[string]bool)
	seenJSON := make(map[string]bool)
	for i := 0; i < typeOf.NumField(); i++ {
		field := typeOf.Field(i)
		if field.Anonymous {
			return nil, nil, fmt.Errorf("connector: setting %s cannot be anonymous", field.Name)
		}
		if field.PkgPath != "" {
			return nil, nil, fmt.Errorf("connector: setting %s must be exported", field.Name)
		}
		if implementsSettingsMarshaler(field.Type) || implementsSettingsMarshaler(reflect.PointerTo(field.Type)) {
			return nil, nil, fmt.Errorf("connector: setting %s cannot implement custom JSON or text marshaling", field.Name)
		}
		parts := strings.Split(field.Tag.Get("connector"), ",")
		kind := strings.TrimSpace(parts[0])
		if kind == "" {
			kind = inferSettingKind(field.Type)
		}
		name := kebab(field.Name)
		jsonParts := strings.Split(field.Tag.Get("json"), ",")
		if len(jsonParts) > 1 {
			return nil, nil, fmt.Errorf("connector: setting %s cannot use JSON tag options", field.Name)
		}
		jsonName := jsonParts[0]
		if jsonName == "" {
			jsonName = field.Name
		}
		if jsonName == "-" {
			return nil, nil, fmt.Errorf("connector: setting %s cannot use json:\"-\"", field.Name)
		}
		entry := settingsField{index: i, name: name, jsonName: jsonName, kind: kind}
		for _, option := range parts[1:] {
			key, optionValue, found := strings.Cut(option, "=")
			switch key {
			case "required":
				entry.required = true
			case "name":
				if !found || optionValue == "" {
					return nil, nil, fmt.Errorf("connector: setting %s has empty name", field.Name)
				}
				entry.name = optionValue
			case "default":
				entry.defaultValue = optionValue
			case "enum":
				entry.enum = strings.Split(optionValue, "|")
			case "description":
				entry.description = optionValue
			case "":
			default:
				return nil, nil, fmt.Errorf("connector: setting %s has unknown option %q", field.Name, option)
			}
		}
		if seen[entry.name] {
			return nil, nil, fmt.Errorf("connector: duplicate setting name %q", entry.name)
		}
		seen[entry.name] = true
		if seenJSON[entry.jsonName] {
			return nil, nil, fmt.Errorf("connector: duplicate setting JSON name %q", entry.jsonName)
		}
		seenJSON[entry.jsonName] = true
		if err := validateSettingType(kind, field.Type); err != nil {
			return nil, nil, fmt.Errorf("connector: setting %s: %w", field.Name, err)
		}
		if entry.defaultValue != "" {
			if err := setSetting(value.Elem().Field(i), entry, entry.defaultValue); err != nil {
				return nil, nil, fmt.Errorf("connector: setting %s default: %w", field.Name, err)
			}
		}
		descriptors = append(descriptors, protocol.SettingDescriptor{Name: entry.name, Kind: entry.kind, Description: entry.description, Required: entry.required, Default: entry.defaultValue, Enum: append([]string(nil), entry.enum...)})
		fields = append(fields, entry)
	}
	if err := protocol.ValidateSettings(descriptors); err != nil {
		return nil, nil, err
	}
	return descriptors, fields, nil
}

func implementsSettingsMarshaler(value reflect.Type) bool {
	jsonMarshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	jsonUnmarshaler := reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	textMarshaler := reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	textUnmarshaler := reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
	return value.Implements(jsonMarshaler) || value.Implements(jsonUnmarshaler) || value.Implements(textMarshaler) || value.Implements(textUnmarshaler)
}

func inferSettingKind(value reflect.Type) string {
	if value == reflect.TypeOf(Secret("")) {
		return "secret"
	}
	if value == reflect.TypeOf(time.Duration(0)) {
		return "duration"
	}
	switch value.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int32, reflect.Int64:
		return "integer"
	default:
		return "unsupported"
	}
}

func validateSettingType(kind string, value reflect.Type) error {
	switch value.Kind() {
	case reflect.Int, reflect.Uint, reflect.Uintptr:
		return fmt.Errorf("architecture-sized integer type %s is unsupported; use a fixed-width signed integer type", value)
	}
	switch kind {
	case "string", "url", "file", "directory", "enum":
		if value.Kind() != reflect.String || value == reflect.TypeOf(Secret("")) {
			return fmt.Errorf("kind %s requires string", kind)
		}
	case "secret":
		if value != reflect.TypeOf(Secret("")) {
			return errors.New("kind secret requires connector.Secret")
		}
	case "bool":
		if value.Kind() != reflect.Bool {
			return errors.New("kind bool requires bool")
		}
	case "integer":
		if value.Kind() < reflect.Int || value.Kind() > reflect.Int64 || value == reflect.TypeOf(time.Duration(0)) {
			return errors.New("kind integer requires an integer type")
		}
	case "duration":
		if value != reflect.TypeOf(time.Duration(0)) {
			return errors.New("kind duration requires time.Duration")
		}
	default:
		return fmt.Errorf("unsupported kind %q", kind)
	}
	return nil
}

func setSetting(value reflect.Value, field settingsField, raw string) error {
	switch field.kind {
	case "string", "url", "file", "directory", "enum", "secret":
		value.SetString(raw)
	case "bool":
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		value.SetBool(parsed)
	case "integer":
		parsed, err := strconv.ParseInt(raw, 10, value.Type().Bits())
		if err != nil {
			return err
		}
		value.SetInt(parsed)
	case "duration":
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return err
		}
		value.SetInt(int64(parsed))
	}
	return validateSettingValue(value, field)
}

func validateSettingValue(value reflect.Value, field settingsField) error {
	switch field.kind {
	case "string", "file", "secret", "bool", "integer", "duration":
		return nil
	case "directory":
		if value.String() != "" && !pathIsAbsolute(value.String()) {
			return errors.New("must be an absolute path")
		}
	case "url":
		parsed, err := url.ParseRequestURI(value.String())
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return errors.New("must be an absolute URL")
		}
	case "enum":
		if len(field.enum) == 0 {
			return errors.New("enum requires connector tag enum=a|b")
		}
		for _, allowed := range field.enum {
			if value.String() == allowed {
				return nil
			}
		}
		return fmt.Errorf("must be one of %s", strings.Join(field.enum, ", "))
	default:
		return fmt.Errorf("unsupported kind %q", field.kind)
	}
	return nil
}

func kebab(value string) string {
	var result strings.Builder
	runes := []rune(value)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]) || i+1 < len(runes) && unicode.IsLower(runes[i+1])) {
			result.WriteByte('-')
		}
		result.WriteRune(unicode.ToLower(r))
	}
	return result.String()
}
