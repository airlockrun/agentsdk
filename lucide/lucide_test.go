package lucide

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestIcon(t *testing.T) {
	var rendered strings.Builder
	if err := Icon("send", `size-4" onload="alert(1)`).Render(t.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	got := rendered.String()
	for _, want := range []string{
		`<svg xmlns="http://www.w3.org/2000/svg"`,
		`viewBox="0 0 24 24"`,
		`stroke="currentColor"`,
		`aria-hidden="true"`,
		`focusable="false"`,
		`class="size-4&#34; onload=&#34;alert(1)"`,
		`<path`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered icon missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, `class="size-4" onload=`) {
		t.Fatalf("class escaped into an event attribute: %s", got)
	}
}

func TestIconRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name  string
		icon  string
		class string
		want  string
	}{
		{name: "unknown", icon: "not-a-lucide-icon", class: "size-4", want: `unknown icon "not-a-lucide-icon"`},
		{name: "empty name", class: "size-4", want: `unknown icon ""`},
		{name: "empty class", icon: "send", want: `Icon("send"): class is required`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				got := recover()
				if got == nil || !strings.Contains(got.(string), tt.want) {
					t.Fatalf("panic = %v, want substring %q", got, tt.want)
				}
			}()
			Icon(tt.icon, tt.class)
		})
	}
}

func TestCatalogIntegrity(t *testing.T) {
	icons := iconCatalog()
	if len(icons) < 1500 {
		t.Fatalf("catalog contains %d icons, want at least 1500", len(icons))
	}
	for _, name := range []string{"circle-plus", "download", "search", "send", "settings", "trash-2", "upload"} {
		if _, ok := icons[name]; !ok {
			t.Errorf("catalog is missing %q", name)
		}
	}
	for name, body := range icons {
		lower := strings.ToLower(body)
		for _, forbidden := range []string{"<script", "<foreignobject", " onload=", " onclick="} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("icon %q contains forbidden content %q", name, forbidden)
			}
		}
	}

	license, err := os.ReadFile("UPSTREAM_LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ISC License", "Lucide Icons and Contributors", "The MIT License (MIT)", "Cole Bemis"} {
		if !strings.Contains(string(license), want) {
			t.Errorf("UPSTREAM_LICENSE missing %q", want)
		}
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(license)); got != "b495047bd93a9b06913511076f504daba17d5bbeb3e0650f3bb53a4220329c57" {
		t.Errorf("UPSTREAM_LICENSE checksum = %s", got)
	}
	skillLicense, err := os.ReadFile("../scaffold/skills/lucide/UPSTREAM_LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	if string(skillLicense) != string(license) {
		t.Error("runtime and skill Lucide licenses differ")
	}
}
