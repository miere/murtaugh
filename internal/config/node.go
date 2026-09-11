package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Seeds live in config rather than only in a flag so they survive a reinstall. The node token is
// deliberately not here: in the store it would sit where an agent's own tools can reach it.
type NodeConfig struct {
	// The -gateway flag wins over this list so an operator can redirect a node once without editing
	// its configuration.
	Gateway []string `yaml:"gateway" json:"gateway,omitempty"`
}

// A bad scheme is rejected at startup because at dial time the refusal lands in a patient redial
// loop that retries forever behind one easily missed log line.
func (n NodeConfig) Validate() error {
	var errs []error
	for i, raw := range n.Gateway {
		address := strings.TrimSpace(raw)
		if address == "" {
			errs = append(errs, fmt.Errorf("node.gateway[%d] must not be blank", i))
			continue
		}
		u, err := url.Parse(address)
		if err != nil {
			errs = append(errs, fmt.Errorf("node.gateway[%d] %q is not a valid address: %w", i, address, err))
			continue
		}
		switch u.Scheme {
		case "ws", "wss":
		default:
			errs = append(errs, fmt.Errorf("node.gateway[%d] %q must be ws:// or wss://", i, address))
			continue
		}
		if strings.TrimSpace(u.Host) == "" {
			errs = append(errs, fmt.Errorf("node.gateway[%d] %q names no host", i, address))
		}
	}
	return errors.Join(errs...)
}

func (n NodeConfig) Seeds() []string {
	out := make([]string, 0, len(n.Gateway))
	for _, raw := range n.Gateway {
		if address := strings.TrimSpace(raw); address != "" {
			out = append(out, address)
		}
	}
	return out
}
