package store

import (
	"context"
	"fmt"
	"slices"

	"github.com/miere/murtaugh/internal/config"
)

var nodeSections = []string{config.SectionAgent, config.SectionMCP, config.SectionJob}

var nodeSingletons = []string{config.SingletonChat, config.SingletonDefaults}

type SplitReport struct {
	Copied map[string]int
	Kept   map[string]int
}

func (r SplitReport) Total() int {
	total := 0
	for _, n := range r.Copied {
		total += n
	}
	return total
}

// Copies and deletes nothing: removing the gateway's agent rows would switch off the
// in-process agent path, which is still the shipping default.
func SplitForNode(ctx context.Context, src, dst config.Store) (SplitReport, error) {
	snap, err := src.Snapshot(ctx)
	if err != nil {
		return SplitReport{}, fmt.Errorf("read the combined configuration: %w", err)
	}

	report := SplitReport{Copied: map[string]int{}, Kept: map[string]int{}}
	var forNode config.Snapshot
	for _, item := range snap.Items {
		if slices.Contains(nodeSections, item.Section) {
			forNode.Items = append(forNode.Items, item)
			report.Copied[item.Section]++
			continue
		}
		report.Kept[item.Section]++
	}
	for _, single := range snap.Singletons {
		if slices.Contains(nodeSingletons, single.Key) {
			forNode.Singletons = append(forNode.Singletons, single)
			report.Copied[single.Key]++
			continue
		}
		report.Kept[single.Key]++
	}

	if err := dst.Restore(ctx, forNode); err != nil {
		return SplitReport{}, fmt.Errorf("write the node configuration: %w", err)
	}

	if _, err := dst.Load(ctx, config.Config{Role: config.RoleNode}); err != nil {
		return SplitReport{}, fmt.Errorf("the node configuration is not valid: %w", err)
	}
	gatewayBase := config.Config{
		Role:  config.RoleGateway,
		OAuth: config.OAuthConfig{AppToken: "x", BotToken: "x"},
	}
	if _, err := src.Load(ctx, gatewayBase); err != nil {
		return SplitReport{}, fmt.Errorf("the gateway configuration is not valid after the split: %w", err)
	}
	return report, nil
}
