package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// NodeConfig is the runtime node's own block: where it dials, and nothing else.
//
// # Why the seed address is configuration and not only a flag
//
// #197 gave the node a candidate list whose seed is pinned and never evicted,
// fed from `-gateway`. A flag is fine for one operator on one laptop and wrong
// for an installed daemon: the address survives a reinstall, it is the one thing
// an operator edits when a gateway moves, and a launchd plist is a worse place
// to keep it than the configuration file that already travels with the node.
//
// # Why several, and why they are seeds rather than a preference
//
// #170 Change H says a gateway registers hostname AND IP where available and
// that learned addresses AUGMENT the seed rather than replacing it. A seed list
// is the same rule applied one level up: an operator who knows their gateway by
// two names (a Tailscale name and a LAN address, say) should be able to say both
// rather than pick the one that will be wrong first. The node walks them in
// order; a redirect appends to the list and never removes a seed.
//
// # What is deliberately NOT here
//
// The node TOKEN. It is a file — <BaseDir>/node-token, owned by the node process
// and denied to the sandboxed model by path — and moving it into the config
// store would put the credential inside the database an agent's own tools can
// be pointed at. See internal/nodetoken.
type NodeConfig struct {
	// Gateway is the ordered seed addresses this node dials, ws:// or wss://.
	//
	// Empty means "told at the command line or not at all": the node still
	// accepts -gateway, which wins over this list so an operator can redirect a
	// node once without editing its configuration.
	Gateway []string `yaml:"gateway" json:"gateway,omitempty"`
}

// Validate checks the seed addresses parse as websocket URLs.
//
// It rejects a bad scheme HERE rather than at dial time because the dialler's
// refusal arrives inside a redial loop that is designed to be patient: a node
// pointed at "https://gateway" would back off and retry forever, and the log
// line saying why scrolls past once. A configuration error should stop the
// process at startup with the offending value in the message.
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

// Seeds returns the configured addresses, trimmed and with blanks dropped.
func (n NodeConfig) Seeds() []string {
	out := make([]string, 0, len(n.Gateway))
	for _, raw := range n.Gateway {
		if address := strings.TrimSpace(raw); address != "" {
			out = append(out, address)
		}
	}
	return out
}
