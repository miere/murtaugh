// Package nodeclaim re-reads only the claim set, never the agent: backends latch their tools at
// startup, so changing which agent a node serves still needs a restart.
package nodeclaim
