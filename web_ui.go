package agentsdk

import (
	"context"
	"io"
	"strconv"

	"github.com/a-h/templ"
)

const webUIAssetName = "web-ui-v1.js"
const webUIStyleName = "web-ui-v1.css"

// WebUIOptions configures WebUI. The zero value enables standard integration.
type WebUIOptions struct {
	// DisableNavigation disables embedded Back controls, independently of
	// authentication recovery and current-page reporting.
	DisableNavigation bool
}

// WebUI renders the SDK's external browser integration script and presentation
// stylesheet. Include it once in each full-page head, not in partial responses.
// It does not load htmx or app theme CSS.
func WebUI(options WebUIOptions) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := io.WriteString(w, `<link rel="stylesheet" href="`+assetsPathPrefix+webUIStyleName+`"/><script src="`+assetsPathPrefix+webUIAssetName+`" data-airlock-disable-navigation="`+templ.EscapeString(strconv.FormatBool(options.DisableNavigation))+`"></script>`)
		return err
	})
}
