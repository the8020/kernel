package recommended

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"the8020/kernel/cbus/core"
	"the8020/kernel/execution/jobs"
	"the8020/kernel/execution/programs"
	"the8020/kernel/packages"
	"the8020/kernel/services"
)

type recommendedManagement struct {
	services.PackageManagementService
	results []packages.RecommendedPackageResult
	err     error
	called  *bool
}

func (m recommendedManagement) RecommendedPackages() ([]packages.RecommendedPackage, error) {
	return []packages.RecommendedPackage{{ID: "acme/demo", Source: "http://upstream.invalid/acme/demo.git"}}, nil
}

func (m recommendedManagement) InstallRecommendedPackagesFrom(_ context.Context, sources []packages.RecommendedSource) ([]packages.RecommendedPackageResult, error) {
	if !reflect.DeepEqual(sources, []packages.RecommendedSource{{PackageID: "acme/demo", Source: "http://dev.invalid/acme/demo.git"}}) {
		return nil, errors.New("source selection was not forwarded")
	}
	*m.called = true
	return m.results, m.err
}

type sourceProgram struct{ err error }

func (p sourceProgram) ResolveProgram(_ context.Context, id string) (packages.ProgramDefinition, error) {
	if id != "the8020/deployments/recommended-sources" {
		return packages.ProgramDefinition{}, errors.New("wrong program")
	}
	return packages.ProgramDefinition{ID: id, PackageID: "the8020/deployments", Commit: "active", EntrypointURL: "file:///workspace/packages/the8020/deployments/programs/recommended-sources/program.ts"}, nil
}

func (p sourceProgram) Run(_ context.Context, _, _ string, options jobs.Options) (jobs.Record, error) {
	if len(options.Arguments) != 1 {
		return jobs.Record{}, errors.New("missing recommendation list")
	}
	return jobs.Record{Result: []packages.RecommendedSource{{PackageID: "acme/demo", Source: "http://dev.invalid/acme/demo.git"}}}, p.err
}

func TestRecommendedCommandReportsResultsAndFailure(t *testing.T) {
	rows := []packages.RecommendedPackageResult{{PackageID: "acme/demo", Status: "skipped", Reason: "local commits"}}
	for _, failure := range []error{nil, errors.New("activation failed")} {
		called := false
		runner, _ := programs.New(sourceProgram{}, sourceProgram{})
		handler := New(&services.Services{PackageManagement: recommendedManagement{results: rows, err: failure, called: &called}, Runtime: &services.RuntimeServices{Programs: runner}})
		result, err := handler(t.Context(), core.Request{})
		if !called || !reflect.DeepEqual(result["packages"], rows) || (err != nil) != (failure != nil) {
			t.Fatalf("result = %+v, %v", result, err)
		}
		if err != nil {
			var commandError *core.Error
			if !errors.As(err, &commandError) || !reflect.DeepEqual(commandError.Details["packages"], rows) {
				t.Fatalf("failure lost package results: %v", err)
			}
		}
	}
	called := false
	runner, _ := programs.New(sourceProgram{}, sourceProgram{err: errors.New("no development connection")})
	if _, err := New(&services.Services{PackageManagement: recommendedManagement{called: &called}, Runtime: &services.RuntimeServices{Programs: runner}})(t.Context(), core.Request{}); err == nil || called {
		t.Fatal("failed source selection must not install from upstream")
	}
	if _, err := New(&services.Services{})(t.Context(), core.Request{}); err == nil {
		t.Fatal("unavailable package management accepted")
	}
}
