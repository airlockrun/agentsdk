package jsexec

import "testing"

func TestOptions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Options)
		valid  bool
	}{
		{"explicit_limits", func(*Options) {}, true},
		{"zero_limits", func(o *Options) { o.Limits = Limits{} }, false},
		{"unbounded_frame", func(o *Options) { o.Limits.FrameBytes = 2 << 20 }, false},
		{"zero_concurrency", func(o *Options) { o.Limits.ConcurrentCalls = 0 }, false},
		{"zero_timeout", func(o *Options) { o.Limits.ExecutionMS = 0 }, false},
		{"duplicate_names", func(o *Options) {
			o.Bindings = []Binding{{Name: "x", Path: []string{"a"}}, {Name: "x", Path: []string{"b"}}}
		}, false},
		{"namespace_collision", func(o *Options) {
			o.Bindings = []Binding{{Name: "x", Path: []string{"a", "b"}}, {Name: "y", Path: []string{"a"}}}
		}, false},
		{"reserved_root", func(o *Options) { o.Bindings = []Binding{{Name: "x", Path: []string{"await"}}} }, false},
		{"user_root", func(o *Options) { o.Bindings = []Binding{{Name: "x", Path: []string{"user", "id"}}} }, false},
		{"log_intrinsic", func(o *Options) { o.Bindings = []Binding{{Name: "x", Path: []string{"air", "log"}}} }, false},
		{"log_descendant", func(o *Options) { o.Bindings = []Binding{{Name: "x", Path: []string{"air", "log", "x"}}} }, false},
		{"air_root", func(o *Options) { o.Bindings = []Binding{{Name: "x", Path: []string{"air"}}} }, false},
		{"user", func(o *Options) { o.User = &User{ID: "caller", DisplayName: "Alice"} }, true},
		{"empty_user", func(o *Options) { o.User = &User{} }, false},
		{"prototype_path", func(o *Options) { o.Bindings = []Binding{{Name: "x", Path: []string{"a", "__proto__"}}} }, false},
		{"namespace", func(o *Options) {
			o.Bindings = []Binding{{Name: "x", Path: []string{"air", "a"}}, {Name: "y", Path: []string{"air", "b"}}}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := Options{Limits: DefaultLimits()}
			tc.change(&o)
			err := o.validate()
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
