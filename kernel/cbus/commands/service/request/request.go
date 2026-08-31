// Package request implements service.request.
package request

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"the8020/kernel/cbus/commands/internal/commandutil"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
	"the8020/kernel/webservices"
)

func New(serviceSet *services.Services) core.Handler {
	return func(ctx context.Context, request core.Request) (core.Result, error) {
		runtimeServices, err := commandutil.Runtime(serviceSet)
		if err != nil {
			return nil, err
		}
		if runtimeServices.Services == nil {
			return nil, core.NewError(core.CodeRuntimeUnavailable, "service manager is unavailable")
		}
		headers, err := parseHeaders(commandutil.String(request, "headers"))
		if err != nil {
			return nil, err
		}
		body, err := requestBody(request, headers)
		if err != nil {
			return nil, err
		}
		result, err := runtimeServices.Services.Request(ctx, commandutil.String(request, "service_id"), commandutil.String(request, "method"), commandutil.String(request, "relative_path"), webservices.RequestOptions{
			Headers: headers,
			Body:    body,
			Timeout: commandutil.Duration(request, "timeout"),
		})
		if err != nil {
			return nil, commandutil.OperationError(err)
		}
		return core.Result{"response": result}, nil
	}
}

func parseHeaders(raw string) (http.Header, error) {
	headers := make(http.Header)
	if strings.TrimSpace(raw) == "" {
		return headers, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	var values map[string]any
	if err := decoder.Decode(&values); err != nil {
		return nil, core.NewError(core.CodeInvalidArguments, fmt.Sprintf("headers must be a JSON object: %v", err))
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, core.NewError(core.CodeInvalidArguments, "headers must contain one JSON object")
	}
	for name, value := range values {
		switch typed := value.(type) {
		case string:
			headers.Set(name, typed)
		case []any:
			for _, item := range typed {
				text, ok := item.(string)
				if !ok {
					return nil, core.NewError(core.CodeInvalidArguments, "header values must be strings")
				}
				headers.Add(name, text)
			}
		default:
			return nil, core.NewError(core.CodeInvalidArguments, "header values must be strings or arrays of strings")
		}
	}
	return headers, nil
}

func requestBody(request core.Request, headers http.Header) (io.Reader, error) {
	textPresent, jsonPresent := commandutil.Has(request, "body"), commandutil.Has(request, "json")
	if textPresent && jsonPresent {
		return nil, core.NewError(core.CodeInvalidArguments, "--body and --json are mutually exclusive")
	}
	if jsonPresent {
		raw := commandutil.String(request, "json")
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return nil, core.NewError(core.CodeInvalidArguments, fmt.Sprintf("json must be valid JSON: %v", err))
		}
		if headers.Get("Content-Type") == "" {
			headers.Set("Content-Type", "application/json")
		}
		return bytes.NewBufferString(raw), nil
	}
	if textPresent {
		return strings.NewReader(commandutil.String(request, "body")), nil
	}
	return nil, nil
}
