package app

import (
	"reflect"
	"testing"
	"time"

	"the8020/kernel/execution"
	"the8020/kernel/execution/programs"
)

func TestNativeGitSecretsUseOwningPackageProgram(t *testing.T) {
	jobRunner := &authenticationJobs{result: map[string]any{"secret": map[string]any{"value": "private-credential"}}}
	runner, err := programs.New(jobRunner, jobRunner)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &packageSecrets{context: t.Context(), programs: runner}
	value, err := resolver.SecretValue("github")
	if err != nil || value != "private-credential" {
		t.Fatal("package secret retrieval failed")
	}
	options := jobRunner.options
	if options.Origin.ID != "the8020/secrets/get" || !reflect.DeepEqual(options.Arguments, []any{"github"}) ||
		options.User != execution.SystemUser() || options.Timeout != 30*time.Second || options.Secrets != nil || options.PlacementGroup != nil {
		t.Fatal("secret resolver bypassed ordinary system-program policy")
	}
	for _, result := range []any{nil, "private-credential", map[string]any{"secret": map[string]any{"value": ""}}} {
		jobRunner.result = result
		if _, err := resolver.SecretValue("github"); err == nil {
			t.Fatal("invalid package result accepted")
		}
	}
}
