package store

import (
	"context"
	"fmt"
)

func verifySchemaV20(ctx context.Context, q schemaInspector) error {
	if err := verifyIndex(ctx, q, "topology_edges", "topology_edges_closed_latest",
		[]string{"valid_to", "child_node"}, 0, 1, "valid_to is not null"); err != nil {
		return fmt.Errorf("store: schema v20 attestation: %w", err)
	}
	return nil
}
