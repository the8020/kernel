package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"the8020/kernel/logging/records"
)

const logQueryPath = "/__the8020/node/logs"
const maximumLogQueryBytes = 16 << 10
const logQueryReadTimeout = 2 * time.Second

type LogReader interface {
	Query(context.Context, records.Query) (records.Page, error)
}

type logQueryResponse struct {
	NodeID string       `json:"node_id"`
	Page   records.Page `json:"page"`
}

func (m *Manager) SetLogReader(reader LogReader) {
	m.mu.Lock()
	m.logs = reader
	m.mu.Unlock()
}

// QueryLogs reads one exact node. It neither searches other nodes nor retries
// elsewhere when an execution's owner is unavailable.
func (m *Manager) QueryLogs(ctx context.Context, query records.Query) (records.Page, error) {
	if query.NodeID == "" {
		query.NodeID = m.localID
	}
	query, err := query.Normalize()
	if err != nil {
		return records.Page{}, err
	}
	if query.NodeID == m.localID {
		m.mu.RLock()
		reader := m.logs
		m.mu.RUnlock()
		if reader == nil {
			return unavailableLogs("The node's log reader is unavailable."), nil
		}
		return reader.Query(ctx, query)
	}
	node, err := m.Inspect(query.NodeID)
	if err != nil || !node.Enabled {
		return unavailableLogs("The node that owns these logs is unavailable."), nil
	}
	body, err := json.Marshal(query)
	if err != nil || len(body) > maximumLogQueryBytes {
		return records.Page{}, errors.New("log query exceeds its request limit")
	}
	target := "http://" + net.JoinHostPort(node.RecipientAddress, strconv.Itoa(node.RecipientPort)) + logQueryPath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return records.Page{}, err
	}
	request.Header.Set("Authorization", "Bearer "+m.secret)
	request.Header.Set("Content-Type", "application/json")
	response, err := m.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return records.Page{}, ctx.Err()
		}
		return unavailableLogs("The node that owns these logs could not be reached."), nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return unavailableLogs("The node could not read these logs."), nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, records.MaxControlFrame+1))
	if err != nil {
		if ctx.Err() != nil {
			return records.Page{}, ctx.Err()
		}
		return unavailableLogs("The node's log response could not be read."), nil
	}
	if len(data) > records.MaxControlFrame {
		return unavailableLogs("The node's log response exceeded its byte limit."), nil
	}
	var result logQueryResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || decoder.Decode(&struct{}{}) != io.EOF || result.NodeID != query.NodeID {
		return unavailableLogs("The node returned an invalid log response."), nil
	}
	if len(result.Page.Records) > query.Limit || len(result.Page.Cursor) > 4096 || (result.Page.State != "ok" && result.Page.State != "expired" && result.Page.State != "unavailable") {
		return unavailableLogs("The node returned an invalid log page."), nil
	}
	for _, item := range result.Page.Records {
		if !query.Filter.Matches(item.Record) {
			return unavailableLogs("The node returned records outside the requested log view."), nil
		}
	}
	return result.Page, nil
}

func unavailableLogs(reason string) records.Page {
	return records.Page{State: "unavailable", Reason: reason, Records: []records.LocatedRecord{}}
}

func (m *Manager) serveLogs(writer http.ResponseWriter, request *http.Request) {
	// These bounds apply only to the small log-control endpoint; forwarded
	// application streams retain their own lifetime and backpressure contracts.
	control := http.NewResponseController(writer)
	if err := control.SetReadDeadline(time.Now().Add(logQueryReadTimeout)); err != nil {
		http.Error(writer, "log request deadline unavailable", http.StatusInternalServerError)
		return
	}
	if err := control.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		http.Error(writer, "log response deadline unavailable", http.StatusInternalServerError)
		return
	}
	var query records.Query
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, maximumLogQueryBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&query); err != nil || decoder.Decode(&struct{}{}) != io.EOF || query.NodeID != m.localID {
		http.Error(writer, "invalid log query", http.StatusBadRequest)
		return
	}
	query, err := query.Normalize()
	if err != nil {
		http.Error(writer, "invalid log query", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2500*time.Millisecond)
	defer cancel()
	page, err := m.QueryLogs(ctx, query)
	if err != nil {
		http.Error(writer, "log query failed", http.StatusBadRequest)
		return
	}
	data, err := json.Marshal(logQueryResponse{NodeID: m.localID, Page: page})
	if err != nil || len(data) > records.MaxControlFrame {
		http.Error(writer, "log response exceeds limit", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(data)
}
