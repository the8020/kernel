// Package core defines the transport-independent typed command bus.
package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
)

const ProtocolVersion = 1

const (
	CodeUnknownCommand      = "unknown_command"
	CodeInvalidArguments    = "invalid_arguments"
	CodeUnknownSetting      = "unknown_setting"
	CodeInvalidSettingValue = "invalid_setting_value"
	CodeNotRuntimeMutable   = "setting_not_runtime_mutable"
	CodePortUnavailable     = "port_unavailable"
	CodeLoggingInit         = "logging_initialization_failure"
	CodePersistence         = "persistence_failure"
	CodeDuplicateInstance   = "duplicate_kernel_instance"
	CodeShuttingDown        = "kernel_shutting_down"
	CodeRuntimeUnavailable  = "runtime_unavailable"
	CodeNotFound            = "not_found"
	CodeConflict            = "conflict"
	CodeTimeout             = "timeout"
	CodeRuntimeOperation    = "runtime_operation_failed"
	CodeInternal            = "internal_error"
)

// Parameter describes one typed positional argument or named command option.
type Parameter struct {
	Name        string `toml:"name"`
	Type        string `toml:"type"`
	Description string `toml:"description"`
	// Option is the long option name without the leading "--". An empty option
	// makes the parameter positional and Position defines its ordering.
	Option   string `toml:"option"`
	Position int    `toml:"position"`
	Required bool   `toml:"required"`
	// Prompt lets administrative clients request an omitted required positional
	// value. Prompted values remain ordinary typed arguments and are echoed.
	Prompt string `toml:"prompt"`
	// Secret parameters are acquired by the administrative client after normal
	// token parsing. They can only be prompted securely or read through their
	// explicit standard-input flag; they are never positional command tokens.
	Secret                   bool   `toml:"secret"`
	SecretPrompt             string `toml:"secret_prompt"`
	SecretConfirmationPrompt string `toml:"secret_confirmation_prompt"`
	SecretStdinOption        string `toml:"secret_stdin_option"`
}

// ResultField describes one field returned by a command.
type ResultField struct{ Name, Type string }

// Command is generated from a command.toml definition.
type Command struct {
	Version         int
	ID              string
	Path            []string
	Aliases         [][]string
	Summary         string
	Description     string
	Parameters      []Parameter
	Result          []ResultField
	MutatesState    bool
	RestartBehavior string
	Examples        []string
}

// Request is the typed transport contract used by handlers.
type Request struct {
	ProtocolVersion int            `json:"protocol_version"`
	CommandID       string         `json:"command_id"`
	Arguments       map[string]any `json:"arguments"`
	RequestID       string         `json:"request_id,omitempty"`
}

// Result is a command's structured response value.
type Result map[string]any

// Error is a stable structured command-bus error.
type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// NewError constructs a structured command error.
func NewError(code, message string) *Error { return &Error{Code: code, Message: message} }

// Response is the versioned command-bus response envelope.
type Response struct {
	ProtocolVersion int    `json:"protocol_version"`
	Success         bool   `json:"success"`
	RequestID       string `json:"request_id,omitempty"`
	Result          Result `json:"result,omitempty"`
	Error           *Error `json:"error,omitempty"`
}

// Handler performs one already-validated typed command.
type Handler func(context.Context, Request) (Result, error)

type registered struct {
	command Command
	handler Handler
}

// Registry owns static command dispatch independent of any transport.
type Registry struct {
	commands map[string]registered
	logger   *slog.Logger
}

// NewRegistry creates an empty command registry.
func NewRegistry(logger *slog.Logger) *Registry {
	return &Registry{commands: make(map[string]registered), logger: logger}
}

// Register installs one generated command-handler binding.
func (r *Registry) Register(command Command, handler Handler) error {
	if handler == nil {
		return fmt.Errorf("register %s: nil handler", command.ID)
	}
	if _, exists := r.commands[command.ID]; exists {
		return fmt.Errorf("register duplicate command %s", command.ID)
	}
	r.commands[command.ID] = registered{command: command, handler: handler}
	return nil
}

// Execute validates a request and dispatches it to the registered handler.
func (r *Registry) Execute(ctx context.Context, request Request) Response {
	if request.RequestID == "" {
		request.RequestID = NewRequestID()
	}
	response := Response{ProtocolVersion: ProtocolVersion, RequestID: request.RequestID}
	if request.ProtocolVersion != ProtocolVersion {
		response.Error = NewError(CodeInvalidArguments, "unsupported protocol version")
		return response
	}
	entry, ok := r.commands[request.CommandID]
	if !ok {
		response.Error = NewError(CodeUnknownCommand, "unknown command: "+request.CommandID)
		return response
	}
	typed, err := validateArguments(entry.command, request.Arguments)
	if err != nil {
		response.Error = NewError(CodeInvalidArguments, err.Error())
		return response
	}
	request.Arguments = typed
	result, err := entry.handler(ctx, request)
	if err != nil {
		var commandError *Error
		if errors.As(err, &commandError) {
			response.Error = commandError
		} else {
			response.Error = NewError(CodeInternal, "internal kernel error")
			if r.logger != nil {
				r.logger.Error("command failed", "request_id", request.RequestID, "command_id", request.CommandID, "error", err)
			}
		}
		return response
	}
	response.Success = true
	response.Result = result
	return response
}

func validateArguments(command Command, input map[string]any) (map[string]any, error) {
	if input == nil {
		input = map[string]any{}
	}
	allowed := make(map[string]Parameter, len(command.Parameters))
	for _, parameter := range command.Parameters {
		allowed[parameter.Name] = parameter
		if parameter.Required {
			if _, ok := input[parameter.Name]; !ok {
				return nil, fmt.Errorf("missing required argument %q", parameter.Name)
			}
		}
	}
	typed := make(map[string]any, len(input))
	for name, value := range input {
		parameter, ok := allowed[name]
		if !ok {
			return nil, fmt.Errorf("unknown argument %q", name)
		}
		converted, err := convertArgument(parameter.Type, value)
		if err != nil {
			return nil, fmt.Errorf("invalid argument %q: %w", name, err)
		}
		typed[name] = converted
	}
	return typed, nil
}

func convertArgument(kind string, value any) (any, error) {
	switch kind {
	case "string":
		text, ok := value.(string)
		if !ok {
			return nil, errors.New("must be a string")
		}
		return text, nil
	case "integer":
		switch number := value.(type) {
		case int:
			return int64(number), nil
		case int64:
			return number, nil
		case float64:
			if number != math.Trunc(number) {
				return nil, errors.New("must be an integer")
			}
			return int64(number), nil
		case json.Number:
			parsed, err := number.Int64()
			if err != nil {
				return nil, errors.New("must be an integer")
			}
			return parsed, nil
		case string:
			parsed, err := strconv.ParseInt(number, 10, 64)
			if err != nil {
				return nil, errors.New("must be an integer")
			}
			return parsed, nil
		default:
			return nil, errors.New("must be an integer")
		}
	case "boolean":
		switch boolean := value.(type) {
		case bool:
			return boolean, nil
		case string:
			parsed, err := strconv.ParseBool(boolean)
			if err != nil {
				return nil, errors.New("must be a boolean")
			}
			return parsed, nil
		default:
			return nil, errors.New("must be a boolean")
		}
	default:
		return nil, fmt.Errorf("unsupported parameter type %q", kind)
	}
}

// NewRequestID returns an opaque locally generated request identifier.
func NewRequestID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "request"
	}
	return hex.EncodeToString(value[:])
}

// PathString renders a command path for help and diagnostics.
func PathString(path []string) string { return strings.Join(path, " ") }
