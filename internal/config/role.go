package config

// The zero value is RoleCombined on purpose, so every existing caller keeps today's behaviour
// without naming a role.
type Role string

const (
	RoleCombined Role = ""
	RoleGateway  Role = "gateway"
	RoleNode     Role = "node"
)

// A node reaches Slack only through its gateway; requiring tokens of it would hand the
// workspace's bot token to every laptop that runs an agent.
func (r Role) HoldsSlackCredentials() bool { return r != RoleNode }

// False for a gateway: bodies live on nodes that may be asleep, so its name checks are deferred
// to connect time and a config's validity then depends on who is online.
func (r Role) HoldsAgentProfiles() bool { return r != RoleGateway }

// RoleCombined prints as "combined" because it is stored as "", which would leave a hole in any
// message that printed it raw.
func (r Role) String() string {
	if r == RoleCombined {
		return "combined"
	}
	return string(r)
}
