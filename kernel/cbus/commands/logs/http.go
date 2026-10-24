package logs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"the8020/kernel/logging/records"
	"the8020/kernel/services"
)

// NewHTTP serves the authenticated sandbox ingress, pinned to its hosting node.
func NewHTTP(serviceSet *services.Services) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var query records.Query
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&query) != nil || decoder.Decode(&struct{}{}) != io.EOF {
			http.Error(w, "invalid log query", http.StatusBadRequest)
			return
		}
		nodeID := serviceSet.Instance.UUID
		if query.NodeID != "" && query.NodeID != nodeID {
			http.Error(w, "sandbox logs are restricted to the hosting node", http.StatusBadRequest)
			return
		}
		query.NodeID = nodeID
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		page, err := Query(ctx, serviceSet, query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			NodeID string       `json:"node_id"`
			Page   records.Page `json:"page"`
		}{nodeID, page})
	})
}
