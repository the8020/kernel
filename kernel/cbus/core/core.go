// Package core defines the transport-independent command bus and its immutable
// process-local catalog snapshots.
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const ProtocolVersion = 2

const (
	CommandKindKernel  = "kernel"
	CommandKindPackage = "package"
)

const (
	CodeUnknownCommand      = "unknown_command"
	CodeStaleCatalog        = "stale_catalog"
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
	CodeDatabaseUnavailable = "database_unavailable"
	CodeDatabaseOperation   = "database_operation_failed"
	CodeInternal            = "internal_error"
)

// Parameter describes one typed positional argument or named kernel-command
// option. Package commands deliberately do not use it.
type Parameter struct {
	Name                     string `toml:"name" json:"name"`
	Type                     string `toml:"type" json:"type"`
	Description              string `toml:"description" json:"description"`
	Option                   string `toml:"option" json:"option,omitempty"`
	Position                 int    `toml:"position" json:"position,omitempty"`
	Required                 bool   `toml:"required" json:"required"`
	Prompt                   string `toml:"prompt" json:"prompt,omitempty"`
	Secret                   bool   `toml:"secret" json:"secret,omitempty"`
	SecretPrompt             string `toml:"secret_prompt" json:"secret_prompt,omitempty"`
	SecretConfirmationPrompt string `toml:"secret_confirmation_prompt" json:"secret_confirmation_prompt,omitempty"`
	SecretStdinOption        string `toml:"secret_stdin_option" json:"secret_stdin_option,omitempty"`
}

type ResultField struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// SecretInput is client-side prompting metadata. Values never enter catalogs.
type SecretInput struct {
	Name               string `toml:"name" json:"name"`
	Required           bool   `toml:"required" json:"required"`
	Prompt             string `toml:"prompt" json:"prompt"`
	ConfirmationPrompt string `toml:"confirmation_prompt" json:"confirmation_prompt,omitempty"`
	StdinOption        string `toml:"stdin_option" json:"stdin_option,omitempty"`
}

type CommandOrigin struct {
	PackageID string `json:"package_id,omitempty"`
	Commit    string `json:"commit,omitempty"`
}

// Command is one catalog descriptor. Name is the visible token for dynamic
// package commands; Path is the canonical tokenized path for built-in commands.
type Command struct {
	Version         int           `json:"version"`
	ID              string        `json:"id"`
	Name            string        `json:"name"`
	Kind            string        `json:"kind"`
	Path            []string      `json:"path,omitempty"`
	Summary         string        `json:"summary"`
	Description     string        `json:"description,omitempty"`
	Usage           string        `json:"usage,omitempty"`
	Parameters      []Parameter   `json:"parameters,omitempty"`
	Secrets         []SecretInput `json:"secrets,omitempty"`
	Result          []ResultField `json:"result_fields,omitempty"`
	MutatesState    bool          `json:"mutates_state"`
	RestartBehavior string        `json:"restart_behavior"`
	Examples        []string      `json:"examples,omitempty"`
	Origin          CommandOrigin `json:"origin,omitempty"`
}

// Request carries untouched package argv and secure inputs separately.
// Arguments carries validated values for built-in handlers and private runtime
// operations.
type Request struct {
	ProtocolVersion int               `json:"protocol_version"`
	CommandID       string            `json:"command_id"`
	Argv            []string          `json:"argv"`
	Secrets         map[string]string `json:"secrets,omitempty"`
	RequestID       string            `json:"request_id,omitempty"`
	CatalogRevision string            `json:"catalog_revision,omitempty"`
	Arguments       map[string]any    `json:"arguments,omitempty"`
}

type Result map[string]any

type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string             { return e.Message }
func NewError(code, message string) *Error { return &Error{Code: code, Message: message} }

type OutputEvent struct {
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

type Response struct {
	ProtocolVersion int           `json:"protocol_version"`
	Success         bool          `json:"success"`
	RequestID       string        `json:"request_id,omitempty"`
	CatalogRevision string        `json:"catalog_revision,omitempty"`
	Output          []OutputEvent `json:"output,omitempty"`
	Result          any           `json:"result,omitempty"`
	Error           *Error        `json:"error,omitempty"`
}

type Diagnostic struct {
	PackageID string `json:"package_id,omitempty"`
	Message   string `json:"message"`
}

type Catalog struct {
	ProtocolVersion int          `json:"protocol_version"`
	Revision        string       `json:"revision"`
	Commands        []Command    `json:"commands"`
	Diagnostics     []Diagnostic `json:"diagnostics,omitempty"`
}

// Handler performs one already-validated typed kernel command.
type Handler func(context.Context, Request) (Result, error)

// DynamicHandler executes one raw-argv package command.
type DynamicHandler func(context.Context, Request) (Execution, error)

type Execution struct {
	Result any
	Output []OutputEvent
}

type Registration struct {
	Command Command
	Handler DynamicHandler
}

type registered struct {
	command Command
	execute DynamicHandler
}

type snapshot struct {
	revision    string
	commands    map[string]registered
	catalog     []Command
	diagnostics []Diagnostic
}

// Registry publishes complete immutable snapshots. A command invocation loads
// one pointer and holds no registry lock while handler code runs.
type Registry struct {
	mu         sync.Mutex
	core       map[string]registered
	packages   map[string]registered
	processID  string
	generation uint64
	current    atomic.Pointer[snapshot]
	logger     *slog.Logger
}

func NewRegistry(logger *slog.Logger) *Registry {
	r := &Registry{
		core: map[string]registered{}, packages: map[string]registered{},
		processID: NewRequestID(), logger: logger,
	}
	r.current.Store(&snapshot{revision: r.processID + "-0", commands: map[string]registered{}, catalog: []Command{}})
	return r
}

func (r *Registry) Register(command Command, handler Handler) error {
	if handler == nil {
		return fmt.Errorf("register %s: nil handler", command.ID)
	}
	command = normalizedCommand(command, CommandKindKernel)
	entry := registered{command: command, execute: func(ctx context.Context, request Request) (Execution, error) {
		result, err := handler(ctx, request)
		return Execution{Result: result}, err
	}}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := validateEntry(entry, r.core, r.packages); err != nil {
		return err
	}
	r.core[command.ID] = entry
	r.publishLocked(nil)
	return nil
}

// ReplacePackages atomically replaces every package command fragment while
// retaining the fixed kernel command set.
func (r *Registry) ReplacePackages(registrations []Registration, diagnostics []Diagnostic) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[string]registered, len(registrations))
	for _, registration := range registrations {
		if registration.Handler == nil {
			return fmt.Errorf("register %s: nil handler", registration.Command.ID)
		}
		command := normalizedCommand(registration.Command, CommandKindPackage)
		entry := registered{command: command, execute: registration.Handler}
		if err := validateEntry(entry, r.core, next); err != nil {
			return err
		}
		next[command.ID] = entry
	}
	r.packages = next
	r.publishLocked(diagnostics)
	return nil
}

func (r *Registry) Catalog() Catalog {
	current := r.current.Load()
	return Catalog{
		ProtocolVersion: ProtocolVersion, Revision: current.revision,
		Commands:    cloneCommands(current.catalog),
		Diagnostics: append([]Diagnostic(nil), current.diagnostics...),
	}
}

func (r *Registry) Execute(ctx context.Context, request Request) Response {
	if request.RequestID == "" {
		request.RequestID = NewRequestID()
	}
	current := r.current.Load()
	response := Response{ProtocolVersion: ProtocolVersion, RequestID: request.RequestID, CatalogRevision: current.revision}
	if request.ProtocolVersion != ProtocolVersion {
		response.Error = NewError(CodeInvalidArguments, "unsupported protocol version")
		return response
	}
	entry, ok := current.commands[request.CommandID]
	if !ok {
		response.Error = NewError(CodeUnknownCommand, "unknown command: "+request.CommandID)
		return response
	}
	if request.CatalogRevision != "" && request.CatalogRevision != current.revision {
		response.Error = NewError(CodeStaleCatalog, "command catalog changed")
		return response
	}
	if err := validateSecrets(entry.command, request.Secrets); err != nil {
		response.Error = NewError(CodeInvalidArguments, err.Error())
		return response
	}
	if entry.command.Kind != CommandKindPackage {
		arguments := request.Arguments
		var err error
		if arguments == nil {
			arguments, err = ParseKernelArguments(entry.command, request.Argv)
			if err == nil {
				arguments = mergeKernelSecrets(entry.command, arguments, request.Secrets)
			}
		}
		if err == nil {
			arguments, err = validateArguments(entry.command, arguments)
		}
		if err != nil {
			response.Error = NewError(CodeInvalidArguments, err.Error())
			return response
		}
		request.Arguments = arguments
	}
	execution, err := entry.execute(ctx, request)
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
	response.Success, response.Result = true, execution.Result
	response.Output = append([]OutputEvent(nil), execution.Output...)
	return response
}

func (r *Registry) publishLocked(diagnostics []Diagnostic) {
	commands := make(map[string]registered, len(r.core)+len(r.packages))
	catalog := make([]Command, 0, len(commands))
	for id, entry := range r.core {
		commands[id] = entry
		catalog = append(catalog, cloneCommand(entry.command))
	}
	for id, entry := range r.packages {
		commands[id] = entry
		catalog = append(catalog, cloneCommand(entry.command))
	}
	sort.Slice(catalog, func(i, j int) bool { return catalog[i].Name < catalog[j].Name })
	r.generation++
	r.current.Store(&snapshot{
		revision: r.processID + "-" + strconv.FormatUint(r.generation, 10),
		commands: commands, catalog: catalog,
		diagnostics: append([]Diagnostic(nil), diagnostics...),
	})
}

func validateEntry(entry registered, fixed, changing map[string]registered) error {
	command := entry.command
	if command.ID == "" || command.Name == "" {
		return errors.New("command ID and visible name are required")
	}
	if command.Kind != CommandKindKernel && command.Kind != CommandKindPackage {
		return fmt.Errorf("command %s has invalid kind %q", command.ID, command.Kind)
	}
	if command.Kind == CommandKindPackage && (command.Name == "kernel" || strings.HasPrefix(command.Name, "kernel.")) {
		return fmt.Errorf("package command %q uses reserved kernel namespace", command.Name)
	}
	for _, existing := range []map[string]registered{fixed, changing} {
		if _, ok := existing[command.ID]; ok {
			return fmt.Errorf("register duplicate command ID %s", command.ID)
		}
		for _, candidate := range existing {
			if visibleCollision(command, candidate.command) {
				return fmt.Errorf("register duplicate visible command %s", command.Name)
			}
		}
	}
	return nil
}

func visibleCollision(left, right Command) bool {
	return left.Name == right.Name
}

func normalizedCommand(command Command, fallbackKind string) Command {
	if command.Kind == "" {
		command.Kind = fallbackKind
	}
	if command.Name == "" {
		command.Name = PathString(command.Path)
	}
	if command.Name == "" {
		command.Name = command.ID
	}
	if len(command.Path) == 0 && command.Name != "" {
		command.Path = []string{command.Name}
	}
	command.Path = append([]string(nil), command.Path...)
	command.Parameters = append([]Parameter(nil), command.Parameters...)
	command.Secrets = append([]SecretInput(nil), command.Secrets...)
	command.Result = append([]ResultField(nil), command.Result...)
	command.Examples = append([]string(nil), command.Examples...)
	return command
}

func validateSecrets(command Command, values map[string]string) error {
	allowed := map[string]bool{}
	required := map[string]bool{}
	for _, secret := range command.Secrets {
		allowed[secret.Name], required[secret.Name] = true, secret.Required
	}
	for _, parameter := range command.Parameters {
		if parameter.Secret {
			allowed[parameter.Name], required[parameter.Name] = true, parameter.Required
		}
	}
	for name := range values {
		if !allowed[name] {
			return fmt.Errorf("unknown secure input %q", name)
		}
	}
	for name, isRequired := range required {
		if _, ok := values[name]; isRequired && !ok {
			return fmt.Errorf("missing required secure input %q", name)
		}
	}
	return nil
}

func mergeKernelSecrets(command Command, arguments map[string]any, secrets map[string]string) map[string]any {
	for _, parameter := range command.Parameters {
		if parameter.Secret {
			if value, ok := secrets[parameter.Name]; ok {
				arguments[parameter.Name] = value
			}
		}
	}
	return arguments
}

// ParseKernelArguments parses only generated kernel metadata. Package argv is
// never interpreted by Go.
func ParseKernelArguments(command Command, tokens []string) (map[string]any, error) {
	positionals := make([]Parameter, 0, len(command.Parameters))
	options := map[string]Parameter{}
	for _, parameter := range command.Parameters {
		if parameter.Secret {
			continue
		}
		if parameter.Option == "" {
			positionals = append(positionals, parameter)
		} else {
			options[parameter.Option] = parameter
		}
	}
	sort.Slice(positionals, func(i, j int) bool { return positionals[i].Position < positionals[j].Position })
	values := map[string]any{}
	seen := map[string]bool{}
	position := 0
	parseOptions := true
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if parseOptions && token == "--" {
			parseOptions = false
			continue
		}
		if parseOptions && strings.HasPrefix(token, "--") {
			name, raw, hasValue := strings.Cut(strings.TrimPrefix(token, "--"), "=")
			parameter, ok := options[name]
			if !ok || name == "" {
				return nil, fmt.Errorf("unknown option %q for %s", token, command.Name)
			}
			if seen[name] {
				return nil, fmt.Errorf("option --%s may only be specified once", name)
			}
			seen[name] = true
			if parameter.Type == "boolean" && !hasValue {
				values[parameter.Name] = true
				continue
			}
			if !hasValue {
				if index+1 >= len(tokens) {
					return nil, fmt.Errorf("option --%s requires a %s value", name, parameter.Type)
				}
				index++
				raw = tokens[index]
			}
			value, err := parseToken(parameter.Type, raw)
			if err != nil {
				return nil, fmt.Errorf("invalid --%s: %w", name, err)
			}
			values[parameter.Name] = value
			continue
		}
		if position >= len(positionals) {
			return nil, fmt.Errorf("too many arguments for %s", command.Name)
		}
		parameter := positionals[position]
		position++
		value, err := parseToken(parameter.Type, token)
		if err != nil {
			return nil, fmt.Errorf("invalid <%s>: %w", parameter.Name, err)
		}
		values[parameter.Name] = value
	}
	return values, nil
}

func parseToken(kind, token string) (any, error) {
	switch kind {
	case "string":
		return token, nil
	case "integer":
		value, err := strconv.ParseInt(token, 10, 64)
		if err != nil {
			return nil, errors.New("must be an integer")
		}
		return value, nil
	case "boolean":
		value, err := strconv.ParseBool(token)
		if err != nil {
			return nil, errors.New("must be true or false")
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported type %q", kind)
	}
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

func cloneCommands(source []Command) []Command {
	result := make([]Command, len(source))
	for index := range source {
		result[index] = cloneCommand(source[index])
	}
	return result
}

func cloneCommand(command Command) Command { return normalizedCommand(command, command.Kind) }

func NewRequestID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "request"
	}
	return hex.EncodeToString(value[:])
}

func PathString(path []string) string { return strings.Join(path, " ") }
