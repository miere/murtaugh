// Package nodeserve runs each request on its own goroutine because nodelink acks a frame only once
// its handler returns: a long prompt served inline would block the Cancel meant to stop it.
package nodeserve
