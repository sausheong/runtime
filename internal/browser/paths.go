package browser

import (
	"fmt"
	"strings"
)

// validateNavURL ensures a navigation target is an absolute http(s) URL.
// Egress (which host is reachable) is enforced by the proxy, not here; this is
// only a scheme/shape guard so a non-web URL never reaches Chrome.
func validateNavURL(u string) error {
	if u == "" {
		return fmt.Errorf("url is required")
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("url must start with http:// or https://")
	}
	if strings.ContainsAny(u, " \t\r\n") {
		return fmt.Errorf("url contains whitespace")
	}
	return nil
}
