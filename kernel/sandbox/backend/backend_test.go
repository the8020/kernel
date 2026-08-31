package backend

import (
	"strings"
	"testing"

	"the8020/kernel/sandbox/model"
)

func TestValidateConsoleOptions(t *testing.T) {
	valid := ConsoleOptions{
		Arguments:   []string{"/bin/bash", "-l"},
		Environment: []string{"TERM=xterm-256color", "HOME=/tmp"},
		WorkingDir:  "/",
		Size:        ConsoleSize{Columns: 80, Rows: 24},
	}
	if err := ValidateConsoleOptions(valid); err != nil {
		t.Fatal(err)
	}
	invalid := []ConsoleOptions{
		{Environment: valid.Environment, WorkingDir: valid.WorkingDir, Size: valid.Size},
		{Arguments: []string{"/bin/bash", ""}, Environment: valid.Environment, WorkingDir: valid.WorkingDir, Size: valid.Size},
		{Arguments: valid.Arguments, Environment: []string{"TERM"}, WorkingDir: valid.WorkingDir, Size: valid.Size},
		{Arguments: valid.Arguments, Environment: valid.Environment, WorkingDir: "relative", Size: valid.Size},
		{Arguments: valid.Arguments, Environment: valid.Environment, WorkingDir: valid.WorkingDir, Size: ConsoleSize{Columns: 1, Rows: 24}},
		{Arguments: []string{"/bin/bash", strings.Repeat("x", maximumConsoleArgumentBytes+1)}, Environment: valid.Environment, WorkingDir: valid.WorkingDir, Size: valid.Size},
	}
	for index, options := range invalid {
		if err := ValidateConsoleOptions(options); err == nil {
			t.Fatalf("invalid console options %d were accepted: %#v", index, options)
		}
	}
}

func TestServiceProcessReceivesReservedDependencyModeForInSandboxValidation(t *testing.T) {
	sandbox := model.SandboxSpec{
		SandboxID: "sandbox", RuntimeGroupID: "group", WorkloadType: model.WorkloadService,
		DependencyMode: model.DependencyCachedOnly,
	}
	config := ProcessConfig{NodeID: "node-one", SupervisorHost: "127.0.0.1", SupervisorPort: 8000, InspectorHost: "127.0.0.1", InspectorPort: 9229}
	environment := RuntimeEnvironment(nil, sandbox, config)
	if !containsExact(environment, "NODE_ID=node-one") {
		t.Fatalf("node identity environment missing: %#v", environment)
	}
	if !containsExact(environment, "DEPENDENCY_MODE=cached_only") {
		t.Fatalf("dependency mode environment missing: %#v", environment)
	}
	arguments := DenoProcessArguments([]string{"deno", "run", "--cached-only", "/opt/runtime/supervisor/main.ts"}, sandbox, config)
	if !containsArgumentValue(arguments, "--allow-env=", "DEPENDENCY_MODE") || !containsArgumentValue(arguments, "--allow-env=", "NODE_ID") || !containsExact(arguments, "--allow-run=/usr/bin/deno") {
		t.Fatalf("service supervisor permissions = %#v", arguments)
	}

	sandbox.WorkloadType = model.WorkloadJob
	arguments = DenoProcessArguments([]string{"deno", "run", "--cached-only", "/opt/runtime/supervisor/main.ts"}, sandbox, config)
	if containsExact(arguments, "--allow-run=/usr/bin/deno") {
		t.Fatalf("application workload received run permission: %#v", arguments)
	}
}

func containsExact(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func containsArgumentValue(arguments []string, prefix, expected string) bool {
	for _, argument := range arguments {
		if strings.HasPrefix(argument, prefix) {
			for _, value := range strings.Split(strings.TrimPrefix(argument, prefix), ",") {
				if value == expected {
					return true
				}
			}
		}
	}
	return false
}
