package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"the8020/kernel/auth"
	"the8020/kernel/execution"
	"the8020/kernel/execution/programs"
	"the8020/kernel/webservices"
)

// The development owner is supplied by the authenticated native ingress, never
// by the JSON body. Package authentication still runs in the ordinary Worker.
func sandboxUserAccess(authentication *packageAuthentication, services func() *webservices.Manager) func(http.ResponseWriter, *http.Request, string, string) {
	return func(writer http.ResponseWriter, request *http.Request, username, operation string) {
		writer.Header().Set("Cache-Control", "no-store")
		user, err := execution.UserForUsername(username)
		if err != nil || services() == nil {
			http.Error(writer, "native user access unavailable", http.StatusServiceUnavailable)
			return
		}
		if operation == "token" {
			result, err := authentication.programs.RunWithOptions(request.Context(), authenticationProgram, "", []any{"allowance", username}, nil, programs.Options{User: user, Timeout: 30 * time.Second})
			if err != nil {
				http.Error(writer, "could not issue user allowance", http.StatusUnauthorized)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(result.Value)
			return
		}
		var input struct {
			ServiceID string            `json:"serviceId"`
			Method    string            `json:"method"`
			Path      string            `json:"path"`
			Headers   map[string]string `json:"headers"`
			Body      string            `json:"body"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 2<<20))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || !strings.HasPrefix(input.Path, "/") || strings.HasPrefix(input.Path, "//") {
			http.Error(writer, "invalid service request", http.StatusBadRequest)
			return
		}
		headers := make(http.Header)
		for key, value := range input.Headers {
			headers.Set(key, value)
		}
		token, _ := auth.RequestToken(&http.Request{Header: headers})
		claims, err := authentication.signing.VerifyToken(token)
		if err != nil || claims["sub"] != user.ID {
			http.Error(writer, "invalid user allowance", http.StatusUnauthorized)
			return
		}
		result, err := services().Request(request.Context(), input.ServiceID, input.Method, input.Path, webservices.RequestOptions{Headers: headers, Body: strings.NewReader(input.Body), Timeout: 30 * time.Second, LocalAuthentication: true})
		if err != nil {
			http.Error(writer, "service request failed", http.StatusBadGateway)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(result)
	}
}
