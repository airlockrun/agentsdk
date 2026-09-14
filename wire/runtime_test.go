package wire

import "testing"

func TestAppRuntimeProtocol(t *testing.T) {
	if AppRuntimeProtocol != "airlock.app-runtime.v2" {
		t.Fatalf("protocol = %q", AppRuntimeProtocol)
	}
	for _, value := range []string{"", "airlock.app-runtime.v1", "airlock.app-runtime.v3", AppRuntimeProtocol} {
		t.Run(value, func(t *testing.T) {
			if err := CheckAppRuntimeProtocol(value); (err == nil) != (value == AppRuntimeProtocol) {
				t.Fatalf("protocol %q: %v", value, err)
			}
		})
	}
}
