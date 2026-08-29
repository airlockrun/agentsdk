package connector

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

// Secret is a locally stored sensitive setting. Configure accepts it only from
// a file or standard input so it does not enter shell history or process lists.
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

const settingsSchemaVersion = 1

type persistedSetting struct {
	protocol.SettingDescriptor
	JSONName string `json:"jsonName"`
}

type persistedSettingsSchema struct {
	Version  int                `json:"version"`
	Settings []persistedSetting `json:"settings"`
}

func saveSettingsSchema(path string, descriptors []protocol.SettingDescriptor, fields []settingsField) error {
	if len(descriptors) != len(fields) {
		return errors.New("connector: settings descriptor and field counts differ")
	}
	settings := make([]persistedSetting, len(fields))
	for i := range fields {
		settings[i] = persistedSetting{SettingDescriptor: descriptors[i], JSONName: fields[i].jsonName}
	}
	body, err := json.Marshal(persistedSettingsSchema{Version: settingsSchemaVersion, Settings: settings})
	if err != nil {
		return err
	}
	return atomicWrite(path, body, 0o600)
}

func loadSettingsSchema(path string) (persistedSettingsSchema, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return persistedSettingsSchema{}, err
	}
	var schema persistedSettingsSchema
	if err := strictUnmarshal(body, &schema); err != nil {
		return persistedSettingsSchema{}, fmt.Errorf("connector: decode settings schema: %w", err)
	}
	if schema.Version != settingsSchemaVersion {
		return persistedSettingsSchema{}, fmt.Errorf("connector: unsupported settings schema version %d", schema.Version)
	}
	descriptors := make([]protocol.SettingDescriptor, len(schema.Settings))
	seenJSON := make(map[string]bool, len(schema.Settings))
	for i, setting := range schema.Settings {
		if setting.JSONName == "" || seenJSON[setting.JSONName] {
			return persistedSettingsSchema{}, errors.New("connector: persisted settings schema has invalid JSON field names")
		}
		seenJSON[setting.JSONName] = true
		descriptors[i] = setting.SettingDescriptor
	}
	if err := protocol.ValidateSettings(descriptors); err != nil {
		return persistedSettingsSchema{}, err
	}
	return schema, nil
}

func settingsSchemasEqual(installed persistedSettingsSchema, descriptors []protocol.SettingDescriptor, fields []settingsField) bool {
	if len(installed.Settings) != len(fields) || len(descriptors) != len(fields) {
		return false
	}
	byName := make(map[string]persistedSetting, len(installed.Settings))
	for _, setting := range installed.Settings {
		byName[setting.Name] = setting
	}
	for i, field := range fields {
		setting, found := byName[field.name]
		if !found || setting.JSONName != field.jsonName || !reflect.DeepEqual(setting.SettingDescriptor, descriptors[i]) {
			return false
		}
	}
	return true
}

func migrateSettings(settings any, fields []settingsField, current persistedSettingsSchema, encoded []byte) error {
	var values map[string]json.RawMessage
	if err := strictUnmarshal(encoded, &values); err != nil {
		return fmt.Errorf("connector: decode installed settings for upgrade: %w", err)
	}
	old := make(map[string]persistedSetting, len(current.Settings))
	for _, setting := range current.Settings {
		old[setting.Name] = setting
	}
	target := reflect.ValueOf(settings).Elem()
	for _, field := range fields {
		installed, found := old[field.name]
		if !found || installed.Kind != field.kind {
			continue
		}
		raw, found := values[installed.JSONName]
		if !found {
			continue
		}
		candidate := reflect.New(target.Field(field.index).Type())
		if err := json.Unmarshal(raw, candidate.Interface()); err != nil || !settingValueCompatible(candidate.Elem(), field) {
			continue
		}
		target.Field(field.index).Set(candidate.Elem())
	}
	return nil
}

func settingValueCompatible(value reflect.Value, field settingsField) bool {
	if value.IsZero() {
		return true
	}
	switch field.kind {
	case "url":
		parsed, err := url.ParseRequestURI(value.String())
		return err == nil && parsed.Scheme != "" && parsed.Host != ""
	case "enum":
		for _, allowed := range field.enum {
			if value.String() == allowed {
				return true
			}
		}
		return false
	default:
		return true
	}
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
	descriptors := make([]protocol.SettingDescriptor, 0, typeOf.NumField())
	fields := make([]settingsField, 0, typeOf.NumField())
	seen := make(map[string]bool)
	seenJSON := make(map[string]bool)
	for i := 0; i < typeOf.NumField(); i++ {
		field := typeOf.Field(i)
		if field.PkgPath != "" {
			continue
		}
		parts := strings.Split(field.Tag.Get("connector"), ",")
		kind := strings.TrimSpace(parts[0])
		if kind == "" {
			kind = inferSettingKind(field.Type)
		}
		name := kebab(field.Name)
		jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
		if jsonName == "" {
			jsonName = field.Name
		}
		if jsonName == "-" {
			return nil, nil, fmt.Errorf("connector: setting %s cannot use json:\"-\"", field.Name)
		}
		entry := settingsField{index: i, name: name, jsonName: jsonName, kind: kind}
		for _, option := range parts[1:] {
			key, value, found := strings.Cut(option, "=")
			switch key {
			case "required":
				entry.required = true
			case "name":
				if !found || value == "" {
					return nil, nil, fmt.Errorf("connector: setting %s has empty name", field.Name)
				}
				entry.name = value
			case "default":
				entry.defaultValue = value
			case "enum":
				entry.enum = strings.Split(value, "|")
			case "description":
				entry.description = value
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
			if err := setSetting(reflect.New(field.Type).Elem(), entry, entry.defaultValue); err != nil {
				return nil, nil, fmt.Errorf("connector: setting %s default: %w", field.Name, err)
			}
		}
		descriptors = append(descriptors, protocol.SettingDescriptor{
			Name: entry.name, Kind: entry.kind, Description: entry.description,
			Required: entry.required, Default: entry.defaultValue, Enum: append([]string(nil), entry.enum...),
		})
		fields = append(fields, entry)
	}
	if err := protocol.ValidateSettings(descriptors); err != nil {
		return nil, nil, err
	}
	return descriptors, fields, nil
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

func configureSettings(ctx context.Context, settings any, fields []settingsField, args []string, input io.Reader, output io.Writer, interactive bool, validate func(context.Context) error) error {
	return configureSettingsCommand(ctx, "configure", settings, fields, args, input, output, interactive, validate)
}

func configureSettingsCommand(ctx context.Context, command string, settings any, fields []settingsField, args []string, input io.Reader, output io.Writer, interactive bool, validate func(context.Context) error) error {
	current, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	copyValue := reflect.New(reflect.ValueOf(settings).Elem().Type())
	if err := json.Unmarshal(current, copyValue.Interface()); err != nil {
		return err
	}
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	set.SetOutput(output)
	nonInteractive := set.Bool("non-interactive", false, "fail rather than prompt")
	values := make(map[string]*string)
	secretStdin := make(map[string]*bool)
	for _, field := range fields {
		if field.kind == "secret" {
			values[field.name] = set.String(field.name+"-file", "", "read secret from file")
			secretStdin[field.name] = set.Bool(field.name+"-stdin", false, "read secret from standard input")
		} else {
			values[field.name] = set.String(field.name, "", field.description)
		}
	}
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return fmt.Errorf("connector: %s takes no positional arguments", command)
	}
	providedFlags := make(map[string]bool)
	set.Visit(func(value *flag.Flag) { providedFlags[value.Name] = true })
	reader := bufio.NewReader(input)
	for _, field := range fields {
		var raw string
		provided := false
		if field.kind == "secret" {
			file := *values[field.name]
			stdin := *secretStdin[field.name]
			if file != "" && stdin {
				return fmt.Errorf("connector: --%s-file and --%s-stdin are mutually exclusive", field.name, field.name)
			}
			if file != "" {
				body, err := os.ReadFile(file)
				if err != nil {
					return fmt.Errorf("read --%s-file: %w", field.name, err)
				}
				raw, provided = trimLineEnding(string(body)), true
			} else if stdin {
				body, err := io.ReadAll(io.LimitReader(reader, 1024*1024+1))
				if err != nil || len(body) > 1024*1024 {
					return fmt.Errorf("read --%s-stdin: secret exceeds 1 MiB or could not be read", field.name)
				}
				raw, provided = trimLineEnding(string(body)), true
			}
		} else if providedFlags[field.name] {
			raw, provided = *values[field.name], true
		}
		fieldValue := copyValue.Elem().Field(field.index)
		if !provided && isZero(fieldValue) && field.defaultValue != "" {
			raw, provided = field.defaultValue, true
		}
		if !provided && interactive && !*nonInteractive && field.kind != "secret" {
			_, _ = fmt.Fprintf(output, "%s: ", field.name)
			line, err := reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			raw = strings.TrimSpace(line)
			provided = raw != ""
		}
		if provided {
			if err := setSetting(fieldValue, field, raw); err != nil {
				return fmt.Errorf("connector: setting %s: %w", field.name, err)
			}
		}
		if field.required && isZero(fieldValue) {
			if field.kind == "secret" {
				return fmt.Errorf("connector: required setting %s is missing; provide --%s-file or --%s-stdin", field.name, field.name, field.name)
			}
			return fmt.Errorf("connector: required setting %s is missing; provide --%s", field.name, field.name)
		}
	}
	original := reflect.ValueOf(settings).Elem()
	proposed := copyValue.Elem()
	original.Set(proposed)
	if validate != nil {
		if err := validate(ctx); err != nil {
			var restored any = reflect.New(original.Type()).Interface()
			_ = json.Unmarshal(current, restored)
			original.Set(reflect.ValueOf(restored).Elem())
			return fmt.Errorf("connector: configuration self-test: %w", err)
		}
	}
	return nil
}

func trimLineEnding(value string) string {
	value = strings.TrimSuffix(value, "\n")
	return strings.TrimSuffix(value, "\r")
}

func setSetting(value reflect.Value, field settingsField, raw string) error {
	switch field.kind {
	case "string", "file", "directory", "secret":
		value.SetString(raw)
	case "url":
		parsed, err := url.ParseRequestURI(raw)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return errors.New("must be an absolute URL")
		}
		value.SetString(raw)
	case "enum":
		if len(field.enum) == 0 {
			return errors.New("enum requires connector tag enum=a|b")
		}
		for _, allowed := range field.enum {
			if raw == allowed {
				value.SetString(raw)
				return nil
			}
		}
		return fmt.Errorf("must be one of %s", strings.Join(field.enum, ", "))
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
	return nil
}

func isZero(value reflect.Value) bool { return value.IsZero() }

func kebab(value string) string {
	var result strings.Builder
	for i, r := range value {
		if i > 0 && r >= 'A' && r <= 'Z' {
			result.WriteByte('-')
		}
		result.WriteRune(r)
	}
	return strings.ToLower(result.String())
}
