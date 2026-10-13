// Package audit records operator actions in the append-only audit log:
// who did what, to which record, when, and why.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/stan-ley-tech/axene-pay/internal/store"
)

// ErrNoActor means an action was attempted without an identified operator.
var ErrNoActor = errors.New("audit: an identified operator is required")

// Record writes one audit entry inside the caller's transaction (q), so the
// entry exists if and only if the action committed.
func Record(ctx context.Context, q *store.Queries, actor, action, targetType, targetID string, detail map[string]any, requestID string) error {
	if strings.TrimSpace(actor) == "" {
		return ErrNoActor
	}
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	return q.InsertAudit(ctx, store.InsertAuditParams{
		Actor: actor, Action: action, TargetType: targetType, TargetID: targetID, Detail: b, RequestID: requestID,
	})
}
