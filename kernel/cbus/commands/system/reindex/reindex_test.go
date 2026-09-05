package reindex

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"the8020/kernel/cbus/core"
	"the8020/kernel/services"
)

func TestCommandRunsTheLivePackageCatalogRefresh(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want []string
	}{
		{"", nil}, {"acme/one, acme/two,acme/one", []string{"acme/one", "acme/two"}},
	} {
		called := false
		serviceSet := &services.Services{}
		serviceSet.PublishRuntime(&services.RuntimeServices{Reindex: func(_ context.Context, ids []string) (core.Result, error) {
			called = true
			if !reflect.DeepEqual(ids, test.want) {
				t.Fatalf("selection=%#v want=%#v", ids, test.want)
			}
			return core.Result{"revision": "process-2", "commands": 7, "events": 3, "hooks": 2}, nil
		}})
		result, err := New(serviceSet)(context.Background(), core.Request{Arguments: map[string]any{"packages": test.raw}})
		if err != nil || !called || result["revision"] != "process-2" || result["commands"] != 7 || result["events"] != 3 || result["hooks"] != 2 {
			t.Fatalf("result=%#v called=%t error=%v", result, called, err)
		}
	}
}

func TestPublicationFailureReachesTheCommandCallerWithPartialPublicationDetails(t *testing.T) {
	serviceSet := &services.Services{}
	result := core.Result{"indexed_packages": []string{"acme/healthy"}}
	serviceSet.PublishRuntime(&services.RuntimeServices{Reindex: func(context.Context, []string) (core.Result, error) {
		return result, errors.New("service index updates were not applied: acme/broken: invalid runtime specification")
	}})
	_, err := New(serviceSet)(context.Background(), core.Request{})
	var exposed *core.Error
	if !errors.As(err, &exposed) || exposed.Code != core.CodeRuntimeOperation || exposed.Message != "service index updates were not applied: acme/broken: invalid runtime specification" || !reflect.DeepEqual(exposed.Details, map[string]any(result)) {
		t.Fatalf("publication failure was hidden: %#v", err)
	}
}
