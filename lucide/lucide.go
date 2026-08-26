// Package lucide renders the complete Lucide icon catalog as inline templ
// components. Icons are decorative; put accessible text on the surrounding
// control, visibly or with an sr-only element.
package lucide

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/a-h/templ"
)

// Version is the bundled Lucide icon catalog version.
const Version = "1.34.0"

const spriteSHA256 = "faa0973e5ee067e776fe669e5429ac501b79f7986ebf006668b253cf5f09cb9d"

//go:embed assets/sprite.svg.gz
var compressedSprite []byte

var (
	iconNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	iconCatalog     = sync.OnceValue(mustLoadIcons)
)

type sprite struct {
	Symbols []symbol `xml:"defs>symbol"`
}

type symbol struct {
	ID      string `xml:"id,attr"`
	ViewBox string `xml:"viewBox,attr"`
	Body    string `xml:",innerxml"`
}

// Icon returns a decorative inline SVG component from the pinned Lucide
// catalog. Name is a kebab-case Lucide icon name and class must size the icon.
// Icon panics on an unknown name or an empty class so misspelled and unstyled
// icons fail during development instead of silently rendering blank space.
func Icon(name, class string) templ.Component {
	body, ok := iconCatalog()[name]
	if !ok {
		panic(fmt.Sprintf("agentsdk/lucide: unknown icon %q", name))
	}
	if strings.TrimSpace(class) == "" {
		panic(fmt.Sprintf("agentsdk/lucide: Icon(%q): class is required", name))
	}
	markup := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false" class="` + templ.EscapeString(class) + `">` + body + `</svg>`
	return templ.Raw(markup)
}

func mustLoadIcons() map[string]string {
	zr, err := gzip.NewReader(bytes.NewReader(compressedSprite))
	if err != nil {
		panic(fmt.Sprintf("agentsdk/lucide: open icon catalog: %v", err))
	}
	data, readErr := io.ReadAll(zr)
	closeErr := zr.Close()
	if readErr != nil {
		panic(fmt.Sprintf("agentsdk/lucide: read icon catalog: %v", readErr))
	}
	if closeErr != nil {
		panic(fmt.Sprintf("agentsdk/lucide: close icon catalog: %v", closeErr))
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != spriteSHA256 {
		panic("agentsdk/lucide: icon catalog checksum mismatch")
	}
	if !bytes.Contains(data, []byte("@license lucide-static v"+Version+" - ISC")) {
		panic("agentsdk/lucide: icon catalog license header does not match Version")
	}

	var catalog sprite
	if err := xml.Unmarshal(data, &catalog); err != nil {
		panic(fmt.Sprintf("agentsdk/lucide: parse icon catalog: %v", err))
	}
	if len(catalog.Symbols) == 0 {
		panic("agentsdk/lucide: icon catalog is empty")
	}

	loaded := make(map[string]string, len(catalog.Symbols))
	for _, icon := range catalog.Symbols {
		if !iconNamePattern.MatchString(icon.ID) {
			panic(fmt.Sprintf("agentsdk/lucide: invalid icon name %q", icon.ID))
		}
		if icon.ViewBox != "0 0 24 24" {
			panic(fmt.Sprintf("agentsdk/lucide: icon %q has viewBox %q", icon.ID, icon.ViewBox))
		}
		if strings.TrimSpace(icon.Body) == "" {
			panic(fmt.Sprintf("agentsdk/lucide: icon %q is empty", icon.ID))
		}
		if _, exists := loaded[icon.ID]; exists {
			panic(fmt.Sprintf("agentsdk/lucide: duplicate icon %q", icon.ID))
		}
		loaded[icon.ID] = icon.Body
	}
	return loaded
}
