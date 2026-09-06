package jobs

import (
	"context"
	"log/slog"
	"time"
)

// Lifecycle records use the observation's timestamp even when publication to
// the logger follows the state lock. No log call performs I/O under that lock.
func (m *Manager) logEvent(record Record, observed time.Time, event, message string, level slog.Level) {
	ctx := context.Background()
	if m.logger == nil || !m.logger.Enabled(ctx, level) {
		return
	}
	entry := slog.NewRecord(observed, level, message, 0)
	entry.AddAttrs(slog.String("component", "jobs"), slog.String("event", event), slog.String("state", record.State))
	for _, field := range []struct{ key, value string }{
		{"node_id", record.NodeID}, {"sandbox_id", record.SandboxID}, {"worker_id", record.WorkerID},
		{"context_id", record.ContextID}, {"parent_context_id", record.ParentContextID}, {"execution_id", record.ExecutionID},
		{"username", record.User.Username}, {"object", string(record.Origin.Type) + ":" + record.Origin.ID},
	} {
		if field.value != "" {
			entry.AddAttrs(slog.String(field.key, field.value))
		}
	}
	if !record.FinishedAt.IsZero() && !record.StartedAt.IsZero() {
		entry.AddAttrs(slog.Duration("duration", record.Duration))
	}
	_ = m.logger.Handler().Handle(ctx, entry)
}
