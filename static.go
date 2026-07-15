package agentsdk

import "net/http"

// StaticAsset declares immutable, in-memory content served publicly from
// /static/{Name}. Name should include a content hash when the bytes can change
// because browsers cache successful responses for one year.
type StaticAsset struct {
	Name        string
	ContentType string
	Data        []byte
}

// RegisterStaticAsset registers a public static asset. Registrations are
// copied, validated, and frozen with the Agent's other declarations.
func (a *Agent) RegisterStaticAsset(asset *StaticAsset) {
	done := a.beginRegistration("RegisterStaticAsset")
	defer done()
	if asset == nil {
		panic("agentsdk: RegisterStaticAsset: nil *StaticAsset")
	}
	cloned := *asset
	if asset.Data != nil {
		cloned.Data = make([]byte, len(asset.Data))
		copy(cloned.Data, asset.Data)
	}
	validateStaticAsset(&cloned)
	if _, exists := a.staticAssets[cloned.Name]; exists {
		panic("agentsdk: duplicate RegisterStaticAsset: " + cloned.Name)
	}
	a.staticAssets[cloned.Name] = &cloned
}

func (a *Agent) handleStaticAsset(w http.ResponseWriter, r *http.Request) {
	asset, ok := a.staticAssets[r.PathValue("name")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", asset.ContentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(asset.Data)
}
