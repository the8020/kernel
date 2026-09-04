// Package cli owns metadata-driven lookup, parsing, help, and rendering.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"the8020/kernel/cbus/core"
)

// Executor is implemented by the typed command-bus client.
type Executor interface {
	Execute(context.Context, core.Request) (core.Response, error)
}

type CatalogProvider interface {
	Catalog(context.Context, string) (core.Catalog, bool, error)
}

// Runner shares all command behavior between one-shot and interactive administration.
type Runner struct {
	commands       []core.Command
	revision       string
	catalog        CatalogProvider
	executor       Executor
	secretResolver SecretResolver
}

// SecretResolver obtains one metadata-declared secret without putting it in
// ordinary command tokens. fromStdin is true only for the explicit flag.
type SecretResolver func(prompt, confirmationPrompt string, fromStdin bool) (string, error)

type localHelpTopic struct {
	path        []string
	usage       string
	summary     string
	description string
}

var localHelpTopics = []localHelpTopic{
	{
		path:        []string{"exit"},
		usage:       "exit",
		summary:     "Exit the interactive console",
		description: "Ends the current admin console session without shutting down the kernel.",
	},
	{
		path:        []string{"help"},
		usage:       "help [command path]",
		summary:     "Show commands or detailed help",
		description: "Lists every available command without a topic, or shows detailed help for one command path.",
	},
}

// New creates a catalog-driven runner.
func New(commands []core.Command, executor Executor) *Runner {
	return &Runner{commands: append([]core.Command(nil), commands...), executor: executor}
}

// NewDynamic creates a runner whose package and kernel command metadata comes
// from the live kernel process.
func NewDynamic(catalog CatalogProvider, executor Executor) *Runner {
	return &Runner{catalog: catalog, executor: executor}
}

// SetSecretResolver configures client-local secret acquisition.
func (r *Runner) SetSecretResolver(resolver SecretResolver) { r.secretResolver = resolver }

// Run resolves, validates, invokes, and renders one command line.
func (r *Runner) Run(ctx context.Context, args []string, jsonOutput bool, output io.Writer) int {
	if err := r.refresh(ctx, false); err != nil {
		renderLocalError(output, err)
		return 1
	}
	if len(args) == 0 {
		_, _ = fmt.Fprintln(output, r.Help(nil))
		return 0
	}
	if args[0] == "help" {
		_, _ = fmt.Fprintln(output, r.Help(args[1:]))
		return 0
	}
	command, consumed, err := r.match(args)
	if err != nil {
		if refreshErr := r.refresh(ctx, true); refreshErr == nil {
			command, consumed, err = r.match(args)
		}
		if err != nil {
			renderLocalError(output, err)
			return 2
		}
	}
	argv, secrets, err := prepareArguments(command, args[consumed:], r.secretResolver)
	if err != nil {
		renderLocalError(output, err)
		return 2
	}
	requestID := core.NewRequestID()
	response, err := r.executor.Execute(ctx, core.Request{ProtocolVersion: core.ProtocolVersion, CommandID: command.ID, Argv: argv, Secrets: secrets, RequestID: requestID, CatalogRevision: r.revision})
	if err != nil {
		renderLocalError(output, err)
		return 1
	}
	if response.Error != nil && (response.Error.Code == core.CodeStaleCatalog || response.Error.Code == core.CodeUnknownCommand) && r.catalog != nil {
		if refreshErr := r.refresh(ctx, true); refreshErr == nil {
			if refreshed, _, matchErr := r.match(args); matchErr == nil {
				response, err = r.executor.Execute(ctx, core.Request{ProtocolVersion: core.ProtocolVersion, CommandID: refreshed.ID, Argv: argv, Secrets: secrets, RequestID: requestID, CatalogRevision: r.revision})
			}
		}
		if err != nil {
			renderLocalError(output, err)
			return 1
		}
	}
	if jsonOutput {
		data, _ := json.MarshalIndent(response, "", "  ")
		_, _ = fmt.Fprintln(output, string(data))
	} else if !response.Success {
		_, _ = fmt.Fprintf(output, "error [%s]: %s\n", response.Error.Code, response.Error.Message)
		if len(response.Error.Details) > 0 {
			renderValue(output, response.Error.Details, 0)
		}
	} else {
		for _, event := range response.Output {
			_, _ = fmt.Fprintln(output, event.Message)
			if len(event.Fields) > 0 {
				renderValue(output, event.Fields, 1)
			}
		}
		if response.Result != nil {
			renderValue(output, response.Result, 0)
		}
	}
	if !response.Success {
		return errorExitCode(response.Error.Code)
	}
	return 0
}

func (r *Runner) refresh(ctx context.Context, force bool) error {
	if r.catalog == nil {
		return nil
	}
	known := r.revision
	if force {
		known = ""
	}
	catalog, unchanged, err := r.catalog.Catalog(ctx, known)
	if err != nil {
		return err
	}
	if unchanged {
		return nil
	}
	r.commands = append([]core.Command(nil), catalog.Commands...)
	r.revision = catalog.Revision
	return nil
}

func (r *Runner) match(tokens []string) (core.Command, int, error) {
	bestLength := 0
	var best core.Command
	for _, command := range r.commands {
		path := command.Path
		if len(path) <= len(tokens) && len(path) > bestLength && equalTokens(path, tokens[:len(path)]) {
			best, bestLength = command, len(path)
		}
	}
	if bestLength == 0 {
		return core.Command{}, 0, fmt.Errorf("unknown command %q; use help to list commands", strings.Join(tokens, " "))
	}
	return best, bestLength, nil
}
func equalTokens(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func prepareArguments(command core.Command, tokens []string, resolveSecret SecretResolver) ([]string, map[string]string, error) {
	metadata := append([]core.SecretInput(nil), command.Secrets...)
	for _, parameter := range command.Parameters {
		if parameter.Secret {
			metadata = append(metadata, core.SecretInput{
				Name: parameter.Name, Required: parameter.Required,
				Prompt:             parameter.SecretPrompt,
				ConfirmationPrompt: parameter.SecretConfirmationPrompt,
				StdinOption:        parameter.SecretStdinOption,
			})
		}
	}
	if len(metadata) == 0 {
		return append([]string(nil), tokens...), nil, nil
	}
	byOption := map[string]core.SecretInput{}
	for _, secret := range metadata {
		if secret.Name == "" {
			return nil, nil, errors.New("command catalog contains an unnamed secure input")
		}
		if secret.StdinOption != "" {
			if _, exists := byOption[secret.StdinOption]; exists {
				return nil, nil, fmt.Errorf("command catalog repeats secure option --%s", secret.StdinOption)
			}
			byOption[secret.StdinOption] = secret
		}
	}
	argv := make([]string, 0, len(tokens))
	fromStdin := map[string]bool{}
	parseOptions := true
	for _, token := range tokens {
		if parseOptions && token == "--" {
			parseOptions = false
			argv = append(argv, token)
			continue
		}
		if parseOptions && strings.HasPrefix(token, "--") {
			nameValue := strings.TrimPrefix(token, "--")
			name, _, hasValue := strings.Cut(nameValue, "=")
			if secret, ok := byOption[name]; ok {
				if hasValue {
					return nil, nil, fmt.Errorf("option --%s does not accept a value", name)
				}
				if fromStdin[secret.Name] {
					return nil, nil, fmt.Errorf("option --%s may only be specified once", name)
				}
				fromStdin[secret.Name] = true
				continue
			}
		}
		argv = append(argv, token)
	}
	values := map[string]string{}
	for _, secret := range metadata {
		if !secret.Required && !fromStdin[secret.Name] {
			continue
		}
		if resolveSecret == nil {
			return nil, nil, fmt.Errorf("%s requires secure input %q", command.Name, secret.Name)
		}
		value, err := resolveSecret(secret.Prompt, secret.ConfirmationPrompt, fromStdin[secret.Name])
		if err != nil {
			return nil, nil, err
		}
		values[secret.Name] = value
	}
	return argv, values, nil
}

// Help renders metadata from the current catalog plus local console commands.
func (r *Runner) Help(path []string) string {
	if len(path) > 0 {
		for _, topic := range localHelpTopics {
			if equalTokens(topic.path, path) {
				return localCommandHelp(topic)
			}
		}
		command, consumed, err := r.match(path)
		if err == nil && consumed == len(path) {
			return commandHelp(command)
		}
		return "unknown help topic: " + strings.Join(path, " ")
	}
	commands := append([]core.Command(nil), r.commands...)
	type summary struct {
		path    string
		usage   string
		summary string
	}
	summaries := make([]summary, 0, len(commands)+len(localHelpTopics))
	for _, command := range commands {
		path := core.PathString(command.Path)
		usage := path
		if command.Usage != "" {
			usage += " " + command.Usage
		}
		summaries = append(summaries, summary{path: path, usage: usage, summary: command.Summary})
	}
	for _, topic := range localHelpTopics {
		summaries = append(summaries, summary{path: core.PathString(topic.path), usage: topic.usage, summary: topic.summary})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].path < summaries[j].path })
	var builder strings.Builder
	builder.WriteString("Commands:\n")
	for _, item := range summaries {
		fmt.Fprintf(&builder, "  %-24s %s\n", item.usage, item.summary)
	}
	builder.WriteString("\nUse help <command path> for details.")
	return builder.String()
}

func localCommandHelp(topic localHelpTopic) string {
	return fmt.Sprintf("Usage: %s\n\n%s\n\n%s", topic.usage, topic.summary, topic.description)
}

func commandHelp(command core.Command) string {
	var builder strings.Builder
	builder.WriteString("Usage: ")
	builder.WriteString(core.PathString(command.Path))
	positionals := make([]core.Parameter, 0, len(command.Parameters))
	options := make([]core.Parameter, 0, len(command.Parameters))
	secureInputs := append([]core.SecretInput(nil), command.Secrets...)
	for _, parameter := range command.Parameters {
		if parameter.Secret {
			secureInputs = append(secureInputs, core.SecretInput{
				Name: parameter.Name, Required: parameter.Required,
				Prompt: parameter.SecretPrompt, ConfirmationPrompt: parameter.SecretConfirmationPrompt,
				StdinOption: parameter.SecretStdinOption,
			})
		} else if parameter.Option == "" {
			positionals = append(positionals, parameter)
		} else {
			options = append(options, parameter)
		}
	}
	sort.Slice(positionals, func(i, j int) bool { return positionals[i].Position < positionals[j].Position })
	sort.Slice(options, func(i, j int) bool { return options[i].Option < options[j].Option })
	if command.Usage != "" {
		builder.WriteByte(' ')
		builder.WriteString(command.Usage)
	} else {
		for _, parameter := range positionals {
			if parameter.Required {
				fmt.Fprintf(&builder, " <%s>", parameter.Name)
			} else {
				fmt.Fprintf(&builder, " [%s]", parameter.Name)
			}
		}
		for _, parameter := range options {
			usage := "--" + parameter.Option
			if parameter.Type != "boolean" {
				usage += " <" + parameter.Name + ">"
			}
			if parameter.Required {
				fmt.Fprintf(&builder, " %s", usage)
			} else {
				fmt.Fprintf(&builder, " [%s]", usage)
			}
		}
		for _, secret := range secureInputs {
			if secret.StdinOption != "" {
				fmt.Fprintf(&builder, " [--%s]", secret.StdinOption)
			}
		}
	}
	fmt.Fprintf(&builder, "\n\n%s\n\n%s", command.Summary, command.Description)
	if len(positionals) > 0 {
		builder.WriteString("\n\nParameters:\n")
		for _, parameter := range positionals {
			fmt.Fprintf(&builder, "  %-16s %-8s %s\n", parameter.Name, parameter.Type, parameter.Description)
		}
	}
	if len(options) > 0 {
		builder.WriteString("\nOptions:\n")
		for _, parameter := range options {
			fmt.Fprintf(&builder, "  %-16s %-8s %s\n", "--"+parameter.Option, parameter.Type, parameter.Description)
		}
	}
	if len(secureInputs) > 0 {
		builder.WriteString("\nSecure inputs:\n")
		for _, secret := range secureInputs {
			label := secret.Name
			if secret.StdinOption != "" {
				label = "--" + secret.StdinOption
			}
			requirement := "optional"
			if secret.Required {
				requirement = "required"
			}
			fmt.Fprintf(&builder, "  %-20s %s (%s)\n", label, secret.Name, requirement)
		}
	}
	if len(command.Examples) > 0 {
		builder.WriteString("\nExamples:\n")
		for _, example := range command.Examples {
			fmt.Fprintf(&builder, "  %s\n", example)
		}
	}
	return strings.TrimRight(builder.String(), "\n")
}

func renderLocalError(output io.Writer, err error) { _, _ = fmt.Fprintf(output, "error: %s\n", err) }
func errorExitCode(code string) int {
	if code == core.CodeInvalidArguments || code == core.CodeUnknownCommand || code == core.CodeUnknownSetting || code == core.CodeInvalidSettingValue {
		return 2
	}
	return 1
}

func renderValue(output io.Writer, value any, indent int) {
	prefix := strings.Repeat("  ", indent)
	switch typed := value.(type) {
	case core.Result:
		renderValue(output, map[string]any(typed), indent)
	case map[string]any:
		if indent == 0 && len(typed) == 1 {
			for key, child := range typed {
				if renderCompactSummaries(output, child) {
					return
				}
				if renderFlatSummaries(output, child) {
					return
				}
				if items, ok := child.([]any); ok && len(items) == 0 {
					_, _ = fmt.Fprintf(output, "%s: none\n", key)
					return
				}
			}
		}
		for _, key := range orderedKeys(typed) {
			child := typed[key]
			if isScalar(child) {
				_, _ = fmt.Fprintf(output, "%s%s: %v\n", prefix, key, child)
			} else {
				_, _ = fmt.Fprintf(output, "%s%s:\n", prefix, key)
				renderValue(output, child, indent+1)
			}
		}
	case []any:
		for _, child := range typed {
			if isScalar(child) {
				_, _ = fmt.Fprintf(output, "%s- %v\n", prefix, child)
			} else {
				_, _ = fmt.Fprintf(output, "%s-\n", prefix)
				renderValue(output, child, indent+1)
			}
		}
	default:
		if isScalar(typed) {
			_, _ = fmt.Fprintf(output, "%s%v\n", prefix, typed)
			return
		}
		data, err := json.Marshal(typed)
		if err == nil {
			var normalized any
			if json.Unmarshal(data, &normalized) == nil {
				renderValue(output, normalized, indent)
				return
			}
		}
		_, _ = fmt.Fprintf(output, "%s%v\n", prefix, typed)
	}
}

func renderCompactSummaries(output io.Writer, value any) bool {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return false
	}
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok || len(fields) != 2 {
			return false
		}
		if _, keyOK := fields["key"].(string); !keyOK {
			return false
		}
		if _, descriptionOK := fields["description"].(string); !descriptionOK {
			return false
		}
	}
	for index, item := range items {
		if index > 0 {
			_, _ = fmt.Fprintln(output)
		}
		fields := item.(map[string]any)
		_, _ = fmt.Fprintln(output, fields["key"])
		_, _ = fmt.Fprintf(output, "  %s\n", fields["description"])
	}
	return true
}

func renderFlatSummaries(output io.Writer, value any) bool {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return false
	}
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok || len(fields) == 0 {
			return false
		}
		for _, field := range fields {
			if !isScalar(field) {
				return false
			}
		}
	}
	for index, item := range items {
		if index > 0 {
			_, _ = fmt.Fprintln(output)
		}
		fields := item.(map[string]any)
		for _, key := range orderedSummaryKeys(fields) {
			_, _ = fmt.Fprintf(output, "%s: %v\n", key, fields[key])
		}
	}
	return true
}

func orderedSummaryKeys(values map[string]any) []string {
	primary := []string{"session_id", "id", "execution_id", "service_id", "package_id", "worker_id", "lease_id", "sandbox_id", "profile_hash", "key"}
	keys := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	primaryKey := ""
	for _, key := range primary {
		if _, exists := values[key]; exists {
			keys = append(keys, key)
			seen[key] = true
			primaryKey = key
			break
		}
	}
	specific := map[string][]string{
		"sandbox_id":   {"workload_type", "state", "worker_count", "warm", "runtime_group_id", "failure"},
		"worker_id":    {"workload_type", "state", "workload_id", "owner_id", "sandbox_id", "in_flight", "failure"},
		"session_id":   {"user_id", "state", "worker_id", "runtime_group_id", "failure"},
		"execution_id": {"job_id", "state", "owner_id", "detached", "duration", "failure"},
		"service_id":   {"description", "canonical_base_path", "state", "enabled", "version_count", "sandbox_count", "worker_count", "validation_error"},
		"package_id":   {"description", "valid", "service_count", "validation_error"},
		"lease_id":     {"protocol", "state", "bind_address", "host_port", "sandbox_id", "internal_port", "purpose", "expires_at"},
		"id":           {"type", "title", "execution_id", "description"},
		"profile_hash": {"desired_warm_count", "ready_warm_count", "creating_count", "reserved_count", "assigned_count", "failed_count", "replenish_count"},
		"key":          {"description", "storage", "configured_value", "active_value", "default_value", "persisted_value", "environment_value", "startup_argument_value", "source", "runtime_mutable", "restart_required", "restart_pending"},
	}
	preferred := append(specific[primaryKey],
		"description", "type", "state", "runtime_group_id", "sandbox_id", "worker_id", "execution_id", "failure", "validation_error")
	for _, key := range preferred {
		if _, exists := values[key]; exists && !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	remaining := make([]string, 0, len(values)-len(keys))
	for key := range values {
		if !seen[key] {
			remaining = append(remaining, key)
		}
	}
	sort.Strings(remaining)
	return append(keys, remaining...)
}

func orderedKeys(values map[string]any) []string {
	preferred := []string{
		"key", "service_id", "package_id", "description", "canonical_base_path", "storage", "configured_value", "active_value", "default_value",
		"persisted_value", "environment_value", "startup_argument_value", "source",
		"runtime_mutable", "restart_required", "restart_pending",
		"instance_uuid", "pid", "instance_root", "uptime", "admin_socket", "main_port", "logging_enabled", "active_log_file", "build_id",
		"database_backend", "database_location", "database_status",
		"database_pool_maximum_open_connections", "database_pool_maximum_idle_connections", "database_pool_open_connections",
		"database_pool_in_use_connections", "database_pool_idle_connections", "database_pool_wait_count", "database_pool_wait_duration_milliseconds", "database_error",
		"ready", "configured_mode", "selected_mode", "selection_reason", "full_ready", "rootless_ready", "capabilities", "limitations", "failure",
		"sandbox_count", "worker_count", "port_count", "warm_pool_profile_count", "warm_pool_desired_count", "warm_pool_ready_count", "warm_pool_failed_count",
		"runtime_ready", "runtime_mode", "runtime_failure",
		"shutdown_requested", "restart_requested", "shutdown_percent", "shutdown_completed_steps", "shutdown_total_steps", "shutdown_step", "shutdown_message",
		"state", "result", "logs", "duration", "resources", "execution_id",
	}
	keys := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, key := range preferred {
		if _, exists := values[key]; exists {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	remaining := make([]string, 0, len(values)-len(keys))
	for key := range values {
		if !seen[key] {
			remaining = append(remaining, key)
		}
	}
	sort.Strings(remaining)
	return append(keys, remaining...)
}

func isScalar(value any) bool {
	switch value.(type) {
	case nil, string, bool, float64, float32, int, int64, int32, uint, uint64, json.Number:
		return true
	default:
		return false
	}
}

// SplitLine tokenizes interactive input with simple quotes and backslash escapes.
func SplitLine(line string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	var quote rune
	escaped := false
	started := false
	flush := func() {
		if started {
			tokens = append(tokens, current.String())
			current.Reset()
			started = false
		}
	}
	for _, character := range line {
		if escaped {
			current.WriteRune(character)
			escaped = false
			started = true
			continue
		}
		if character == '\\' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				current.WriteRune(character)
			}
			started = true
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			started = true
			continue
		}
		if character == ' ' || character == '\t' {
			flush()
			continue
		}
		current.WriteRune(character)
		started = true
	}
	if escaped {
		return nil, errors.New("unfinished escape")
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	flush()
	return tokens, nil
}
