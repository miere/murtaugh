package gateway

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/miere/murtaugh/internal/slack/alertcard"
)

// NodeReport identifies the machine now serving, for the takeover
// announcement. When a failover happens the first question is "which box am I
// talking to?", and an operator cannot answer it from the Slack message unless
// the message says.
type NodeReport struct {
	// Host is the machine's hostname.
	Host string
	// LocalIP is the address of the interface carrying the default route —
	// the one that means something on the operator's own network.
	LocalIP string
	// PublicIP is the address the internet sees. Empty when it could not be
	// determined, which is not an error worth failing a promotion over.
	PublicIP string
	// Version is the running Murtaugh build.
	Version string
	// PID distinguishes two daemons on one host, which is exactly the case the
	// local lock backend exists to catch.
	PID int
}

// publicIPEndpoint is the service asked for this node's outside address.
//
// Reaching for an external service at all is a deliberate, narrow choice: a
// node behind NAT cannot learn its public address any other way, and the draft
// asked for it explicitly because a failover notice naming only a private
// 10.x address does not tell an operator which site took over. The call is
// bounded, made once per promotion, and its failure is invisible — the notice
// simply omits the field.
const publicIPEndpoint = "https://api.ipify.org"

// publicIPTimeout bounds the lookup. A promotion must not wait on a third
// party: the point of failing over quickly is undone by a 30-second DNS stall.
const publicIPTimeout = 3 * time.Second

// resolveNodeReport gathers this node's identity. Every field degrades
// independently; none of them is worth failing a promotion for.
func resolveNodeReport(ctx context.Context, version string, httpGet func(context.Context, string) (string, error)) NodeReport {
	report := NodeReport{Version: version, PID: os.Getpid()}
	if host, err := os.Hostname(); err == nil {
		report.Host = strings.TrimSpace(host)
	}
	report.LocalIP = localIP()
	if httpGet != nil {
		lookupCtx, cancel := context.WithTimeout(ctx, publicIPTimeout)
		defer cancel()
		if ip, err := httpGet(lookupCtx, publicIPEndpoint); err == nil {
			report.PublicIP = strings.TrimSpace(ip)
		}
	}
	return report
}

// localIP returns the address of the interface that would carry outbound
// traffic.
//
// The UDP "dial" sends nothing — connecting a datagram socket only makes the
// kernel choose a route and bind a local address — so this is a routing-table
// lookup rather than a network call, and it works offline. It beats walking
// net.Interfaces(), which cannot tell which of several addresses is the one
// that matters.
func localIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	return addr.IP.String()
}

// fetchPublicIP is the default public-address lookup.
func fetchPublicIP(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	// Deliberately NOT the gateway's gated client: this is not a Slack call,
	// and it happens during promotion when leadership is still being
	// established.
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("public IP lookup returned %s", res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 64))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// promotionAlert is the notice a new leader posts to the admin DM.
//
// It names the epoch and the reason, not just the fact of a takeover. An
// operator reading "took over from an expired lease" learns something different
// from "took over after a clean handover": the first says a node died or was
// partitioned, the second says someone restarted it. Reporting only "I am the
// leader now" would leave them to guess which.
func promotionAlert(report NodeReport, epoch int64, reason string) alertcard.Spec {
	return alertcard.Spec{
		Level:     alertcard.LevelInfo,
		Title:     "Murtaugh failed over to a new node",
		Subtitle:  fmt.Sprintf("%s is now the leader (%s).", nodeLabel(report), reason),
		Detail:    nodeDetail(report, epoch),
		NextSteps: "Any conversation in flight on the previous leader was stopped; ask again to resume it.",
	}
}

// nodeLabel is the short human name for a node.
func nodeLabel(report NodeReport) string {
	if host := strings.TrimSpace(report.Host); host != "" {
		return host
	}
	if ip := strings.TrimSpace(report.LocalIP); ip != "" {
		return ip
	}
	return "this node"
}

// nodeDetail renders the addressing block of the announcement.
func nodeDetail(report NodeReport, epoch int64) string {
	lines := []string{
		"Host: " + orUnknown(report.Host),
		"Local IP: " + orUnknown(report.LocalIP),
		"Public IP: " + orUnknown(report.PublicIP),
	}
	if v := strings.TrimSpace(report.Version); v != "" {
		lines = append(lines, "Version: "+v)
	}
	lines = append(lines, fmt.Sprintf("PID: %d", report.PID))
	// The epoch totally orders takeovers, so two notices can be placed in
	// sequence even when they arrive out of order or one was never delivered.
	lines = append(lines, fmt.Sprintf("Leadership epoch: %d", epoch))
	return strings.Join(lines, "\n")
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unavailable"
	}
	return s
}

// AnnouncePromotion posts the takeover notice to the admin DM.
//
// Best-effort by design: a node that has just won an election and cannot post
// about it is still the leader, and refusing the promotion over a failed
// announcement would trade a cosmetic problem for an outage.
func (a *Gateway) AnnouncePromotion(ctx context.Context, epoch int64, reason string) {
	admin := strings.TrimSpace(a.access().AdminUser)
	if admin == "" {
		return
	}
	resolve := a.nodeReport
	if resolve == nil {
		resolve = func(ctx context.Context) NodeReport {
			return resolveNodeReport(ctx, a.version, fetchPublicIP)
		}
	}
	if _, _, err := a.postLifecycleAlert(ctx, admin, "", promotionAlert(resolve(ctx), epoch, reason)); err != nil {
		a.logger.Warn("could not announce the leadership takeover", "error", err)
	}
}

// WithNodeReporter overrides how this node describes itself in the takeover
// announcement. Tests use it to avoid a real network lookup.
func (a *Gateway) WithNodeReporter(report func(ctx context.Context) NodeReport) *Gateway {
	a.nodeReport = report
	return a
}
