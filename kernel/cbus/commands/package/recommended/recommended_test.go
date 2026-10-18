package recommended

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"the8020/kernel/cbus/core"
	"the8020/kernel/packages"
	"the8020/kernel/services"
)

type recommendedManagement struct {
	services.PackageManagementService
	results []packages.RecommendedPackageResult
	err     error
}

func (m recommendedManagement) InstallRecommendedPackages(context.Context) ([]packages.RecommendedPackageResult, error) {
	return m.results, m.err
}

func TestRecommendedCommandReportsResultsAndFailure(t *testing.T) {
	rows := []packages.RecommendedPackageResult{{PackageID: "acme/demo", Status: "skipped", Reason: "local commits"}}
	for _, failure := range []error{nil, errors.New("activation failed")} {
		handler := New(&services.Services{PackageManagement: recommendedManagement{results: rows, err: failure}})
		result, err := handler(t.Context(), core.Request{})
		if !reflect.DeepEqual(result["packages"], rows) || (err != nil) != (failure != nil) {
			t.Fatalf("result = %+v, %v", result, err)
		}
		if err != nil {
			var commandError *core.Error
			if !errors.As(err, &commandError) || !reflect.DeepEqual(commandError.Details["packages"], rows) {
				t.Fatalf("failure lost package results: %v", err)
			}
		}
	}
	if _, err := New(&services.Services{})(t.Context(), core.Request{}); err == nil {
		t.Fatal("unavailable package management accepted")
	}
}
