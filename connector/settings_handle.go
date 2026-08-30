package connector

import (
	"reflect"
	"sync/atomic"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

// SettingsDefinition is a sealed connector settings declaration created by
// DefineSettings. The interface prevents raw mutable struct pointers from being
// supplied to Config.
type SettingsDefinition interface {
	definition() (any, []protocol.SettingDescriptor, []settingsField)
	publish()
	clear()
	claim(*Runtime)
}

// Settings is a late-bound snapshot of connector settings. Get is available
// only while Runtime is executing a lifecycle command.
type Settings[T any] struct {
	target      *T
	descriptors []protocol.SettingDescriptor
	fields      []settingsField
	current     atomic.Pointer[T]
	owner       atomic.Pointer[Runtime]
}

// DefineSettings declares the connector's settings schema. T must be a struct
// whose configurable fields use connector tags.
func DefineSettings[T any]() *Settings[T] {
	target := new(T)
	descriptors, fields, err := settingsSchema(target)
	if err != nil {
		panic(err)
	}
	return &Settings[T]{target: target, descriptors: descriptors, fields: fields}
}

// Get returns the settings selected for the current lifecycle command.
func (s *Settings[T]) Get() T {
	if s == nil {
		panic("connector: settings are required")
	}
	current := s.current.Load()
	if current == nil {
		panic("connector: settings are unavailable during connector definition")
	}
	return *current
}

// Directory identifies one directory setting for BoundLocalDirectory.
func (s *Settings[T]) Directory(selectField func(*T) *string) DirectorySetting {
	if s == nil || selectField == nil {
		panic("connector: settings and directory field selector are required")
	}
	candidate := new(T)
	selected := selectField(candidate)
	if selected == nil {
		panic("connector: directory field selector returned nil")
	}
	value := reflect.ValueOf(candidate).Elem()
	selectedPointer := reflect.ValueOf(selected).Pointer()
	for _, field := range s.fields {
		if field.kind == "directory" && value.Field(field.index).CanAddr() && value.Field(field.index).Addr().Pointer() == selectedPointer {
			return DirectorySetting{settings: s, field: field}
		}
	}
	panic("connector: directory field selector must return a field declared with connector kind directory")
}

func (s *Settings[T]) definition() (any, []protocol.SettingDescriptor, []settingsField) {
	if s == nil {
		panic("connector: settings are required")
	}
	return s.target, append([]protocol.SettingDescriptor(nil), s.descriptors...), append([]settingsField(nil), s.fields...)
}

func (s *Settings[T]) publish() {
	value := *s.target
	s.current.Store(&value)
}

func (s *Settings[T]) clear() {
	s.current.Store(nil)
}

func (s *Settings[T]) claim(runtime *Runtime) {
	if runtime == nil || !s.owner.CompareAndSwap(nil, runtime) {
		panic("connector: settings handle can belong to only one runtime")
	}
}

// DirectorySetting is an explicit binding to a string setting declared with
// connector kind directory.
type DirectorySetting struct {
	settings SettingsDefinition
	field    settingsField
}
