package config

// A configuration belongs to exactly one binary. There is no combined role and no zero value:
// the binary loading the configuration says which it is, and an unset role fails validation.
type Role string

const (
	RoleGateway Role = "gateway"
	RoleNode    Role = "node"
)

// Valid reports whether a role was set at all. The zero value is not a role.
func (r Role) Valid() bool { return r == RoleGateway || r == RoleNode }

// A node reaches Slack only through its gateway; requiring tokens of it would hand the
// workspace's bot token to every laptop that runs an agent.
func (r Role) HoldsSlackCredentials() bool { return r == RoleGateway }

// False for a gateway: bodies live on nodes that may be asleep, so its name checks are deferred
// to connect time and a config's validity then depends on who is online.
func (r Role) HoldsAgentProfiles() bool { return r == RoleNode }

// The unset role prints as "unset" because it is stored as "", which would leave a hole in any
// message that printed it raw.
func (r Role) String() string {
	if !r.Valid() {
		return "unset"
	}
	return string(r)
}
