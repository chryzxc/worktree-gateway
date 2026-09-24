package registry

import (
	"fmt"
	"strings"

	"github.com/chryzxc/worktree-gateway/internal/config"
)

// URL builds the local URL for a hostname under the gateway settings.
func URL(host string, g config.Global) string {
	if g.HTTPS {
		if g.HTTPSPort == 443 {
			return "https://" + host
		}
		return fmt.Sprintf("https://%s:%d", host, g.HTTPSPort)
	}
	if g.HTTPPort == 80 {
		return "http://" + host
	}
	return fmt.Sprintf("http://%s:%d", host, g.HTTPPort)
}

// EnvName converts a service name to an env-var fragment (api-v2 → API_V2).
func EnvName(service string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(service))
}
