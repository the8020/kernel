package secretcommands_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	secretget "the8020/kernel/cbus/commands/secret/get"
	secretlist "the8020/kernel/cbus/commands/secret/list"
	secretset "the8020/kernel/cbus/commands/secret/set"
	"the8020/kernel/cbus/core"
	"the8020/kernel/secrets"
	"the8020/kernel/services"
)

type fakeSecrets struct {
	items map[string]secrets.Secret
	now   time.Time
}

func (f *fakeSecrets) List() ([]secrets.Summary, error) {
	result := make([]secrets.Summary, 0, len(f.items))
	for _, item := range f.items {
		result = append(result, secrets.Summary{Name: item.Name, UpdatedAt: item.UpdatedAt})
	}
	return result, nil
}

func (f *fakeSecrets) Get(name string) (secrets.Secret, error) {
	return f.items[name], nil
}

func (f *fakeSecrets) Set(_ context.Context, name, value string) (secrets.Summary, error) {
	item := secrets.Secret{Name: name, Value: value, UpdatedAt: f.now}
	f.items[name] = item
	return secrets.Summary{Name: name, UpdatedAt: f.now}, nil
}

func TestSecretHandlersExposeValuesOnlyFromExplicitGet(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	store := &fakeSecrets{items: map[string]secrets.Secret{}, now: now}
	serviceSet := &services.Services{Secrets: store}

	setResult, err := secretset.New(serviceSet)(context.Background(), core.Request{
		Arguments: map[string]any{"name": "github", "value": "private-token"},
	})
	if err != nil {
		t.Fatal(err)
	}
	encodedSet, err := json.Marshal(setResult)
	if err != nil || strings.Contains(string(encodedSet), "private-token") {
		t.Fatalf("set result disclosed value: %s, %v", encodedSet, err)
	}

	listResult, err := secretlist.New(serviceSet)(context.Background(), core.Request{})
	if err != nil {
		t.Fatal(err)
	}
	encodedList, err := json.Marshal(listResult)
	if err != nil || strings.Contains(string(encodedList), "private-token") {
		t.Fatalf("list result disclosed value: %s, %v", encodedList, err)
	}

	getResult, err := secretget.New(serviceSet)(context.Background(), core.Request{
		Arguments: map[string]any{"name": "github"},
	})
	if err != nil {
		t.Fatal(err)
	}
	secret, ok := getResult["secret"].(secrets.Secret)
	if !ok || secret.Name != "github" || secret.Value != "private-token" || !secret.UpdatedAt.Equal(now) {
		t.Fatalf("get result = %#v", getResult)
	}
}
