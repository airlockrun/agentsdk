package agentsdk

import (
	"strings"

	"github.com/airlockrun/agentsdk/internal/binding"
	"github.com/dop251/goja"
)

type jsBindingSet struct {
	vm     *goja.Runtime
	nodes  map[string]*goja.Object
	leaves map[string]struct{}
}

func newJSBindingSet(vm *goja.Runtime) *jsBindingSet {
	return &jsBindingSet{
		vm:     vm,
		nodes:  make(map[string]*goja.Object),
		leaves: make(map[string]struct{}),
	}
}

func (s *jsBindingSet) Set(path binding.Path, value any) {
	parts := path.JSParts()
	if len(parts) < 2 {
		panic("agentsdk: invalid JS capability path")
	}
	for i := 1; i < len(parts); i++ {
		prefix := strings.Join(parts[:i], ".")
		if _, exists := s.leaves[prefix]; exists {
			panic("agentsdk: JS capability leaf used as namespace: " + prefix)
		}
		if _, exists := s.nodes[prefix]; exists {
			continue
		}
		node := s.vm.NewObject()
		if i == 1 {
			if existing := s.vm.Get(parts[0]); existing != nil && !goja.IsUndefined(existing) {
				panic("agentsdk: duplicate JS capability root: " + parts[0])
			}
			if err := s.vm.Set(parts[0], node); err != nil {
				panic(err)
			}
		} else {
			parent := s.nodes[strings.Join(parts[:i-1], ".")]
			if err := parent.Set(parts[i-1], node); err != nil {
				panic(err)
			}
		}
		s.nodes[prefix] = node
	}
	full := strings.Join(parts, ".")
	if _, exists := s.leaves[full]; exists {
		panic("agentsdk: duplicate JS capability binding: " + full)
	}
	if _, exists := s.nodes[full]; exists {
		panic("agentsdk: JS capability namespace used as leaf: " + full)
	}
	parent := s.nodes[strings.Join(parts[:len(parts)-1], ".")]
	if err := parent.Set(parts[len(parts)-1], value); err != nil {
		panic(err)
	}
	s.leaves[full] = struct{}{}
}

func (s *jsBindingSet) SetReservedRoot(name string, value any) {
	if existing := s.vm.Get(name); existing != nil && !goja.IsUndefined(existing) {
		panic("agentsdk: duplicate JS reserved root: " + name)
	}
	if err := s.vm.Set(name, value); err != nil {
		panic(err)
	}
}
