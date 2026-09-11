// Package nodelink keeps delivery outside the payload so a payload change can never break it. A
// frame is acked only once the local Handler returns, not when it is decoded.
package nodelink
