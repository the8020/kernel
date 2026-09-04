// Package server exposes a registry through HTTP/JSON on one Unix socket.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"the8020/kernel/cbus/core"
)

// Server owns only command-bus transport lifecycle.
type Server struct {
	path         string
	registry     *core.Registry
	listener     net.Listener
	http         *http.Server
	mu           sync.RWMutex
	shuttingDown bool
	shutdownRead map[string]bool
	serveError   chan error
}

// New constructs a stopped command-bus server.
func New(path string, registry *core.Registry) *Server {
	return &Server{path: path, registry: registry, shutdownRead: map[string]bool{}, serveError: make(chan error, 1)}
}

// Start binds the restrictive Unix socket and starts serving.
func (s *Server) Start() error {
	previousUmask := syscall.Umask(0o177)
	listener, err := net.Listen("unix", s.path)
	syscall.Umask(previousUmask)
	if err != nil {
		return fmt.Errorf("bind administrative socket: %w", err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		info, statErr := os.Stat(s.path)
		directoryInfo, directoryErr := os.Stat(filepath.Dir(s.path))
		secureSocket := statErr == nil && info.Mode().Perm() == 0o600
		unsupportedButContained := errors.Is(err, syscall.EINVAL) && directoryErr == nil && directoryInfo.Mode().Perm()&0o077 == 0
		if !secureSocket && !unsupportedButContained {
			_ = listener.Close()
			_ = os.Remove(s.path)
			return fmt.Errorf("restrict administrative socket: %w", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/cbus/catalog", s.catalog)
	mux.HandleFunc("POST /v2/cbus/execute", s.execute)
	s.listener = listener
	s.http = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		err := s.http.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.serveError <- err
	}()
	return nil
}

func (s *Server) catalog(writer http.ResponseWriter, request *http.Request) {
	catalog := s.registry.Catalog()
	etag := `"` + catalog.Revision + `"`
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("ETag", etag)
	if request.Header.Get("If-None-Match") == etag {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(catalog)
}

func (s *Server) execute(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	defer request.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.UseNumber()
	var commandRequest core.Request
	if err := decoder.Decode(&commandRequest); err != nil {
		writeResponse(writer, core.Response{ProtocolVersion: core.ProtocolVersion, Error: core.NewError(core.CodeInvalidArguments, "invalid request body")})
		return
	}
	s.mu.RLock()
	stopping := s.shuttingDown
	allowed := s.shutdownRead[commandRequest.CommandID]
	s.mu.RUnlock()
	if stopping && !allowed {
		writeResponse(writer, core.Response{ProtocolVersion: core.ProtocolVersion, Error: core.NewError(core.CodeShuttingDown, "kernel is shutting down")})
		return
	}
	response := s.registry.Execute(request.Context(), commandRequest)
	writeResponse(writer, response)
}

// BeginShutdown rejects new commands except the explicitly supplied progress
// and idempotent lifecycle command IDs while leaving the socket available for
// shutdown observation.
func (s *Server) BeginShutdown(allowedCommandIDs ...string) {
	s.mu.Lock()
	s.shuttingDown = true
	s.shutdownRead = make(map[string]bool, len(allowedCommandIDs))
	for _, id := range allowedCommandIDs {
		if id != "" {
			s.shutdownRead[id] = true
		}
	}
	s.mu.Unlock()
}

func writeResponse(writer http.ResponseWriter, response core.Response) {
	if err := json.NewEncoder(writer).Encode(response); err != nil {
		return
	}
}

// Shutdown stops accepting work and removes the Unix socket.
func (s *Server) Shutdown(ctx context.Context) error {
	s.BeginShutdown()
	if s.http == nil {
		return nil
	}
	err := s.http.Shutdown(ctx)
	if closeErr := s.listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
		err = errors.Join(err, closeErr)
	}
	select {
	case serveErr := <-s.serveError:
		err = errors.Join(err, serveErr)
	default:
	}
	if removeErr := os.Remove(s.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = errors.Join(err, removeErr)
	}
	return err
}
