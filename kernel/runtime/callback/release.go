package callback

import (
	"net/http"

	"the8020/kernel/identity"
	"the8020/kernel/runtime/protocol"
	"the8020/kernel/sandbox/model"
)

// Worker death releases its replaceable kernel resources, not processes that
// explicitly have broker lifetime. This call is made by the trusted supervisor,
// after it cancels the Worker's outstanding calls, including on a Worker crash.
func (s *Server) handleWorkerRelease(writer http.ResponseWriter, _ *http.Request, message protocol.Envelope, spec model.SandboxSpec) {
	var payload struct {
		WorkerID string `json:"worker_id"`
	}
	if err := decodePayload(message.Payload, &payload); err != nil || !identity.Is(payload.WorkerID, "wrk") || message.CorrelationID == "" {
		http.Error(writer, "invalid Worker release identity", http.StatusBadRequest)
		return
	}
	if s.database != nil {
		s.database.CloseScopePrefix(databaseScope(spec.SandboxID, payload.WorkerID))
	}
	s.mu.Lock()
	release := s.releaseWorker
	s.mu.Unlock()
	if release != nil {
		release(spec.SandboxID, payload.WorkerID)
	}
	writeJSON(writer, protocol.Envelope{
		ProtocolVersion: s.protocolVersion, MessageType: protocol.MessageAdminResult,
		SandboxID: spec.SandboxID, CorrelationID: message.CorrelationID,
		Payload: []byte(`{"released":true}`),
	})
}
