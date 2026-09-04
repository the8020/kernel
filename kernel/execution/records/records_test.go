package records

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestRoundTripOrderingModesAndDelete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("b", map[string]any{"value": "old"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("a", map[string]any{"value": "one"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("b", map[string]any{"value": "two"}); err != nil {
		t.Fatal(err)
	}
	ids, err := store.IDs()
	if err != nil || len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("ids=%#v err=%v", ids, err)
	}
	var value struct {
		Value string `json:"value"`
	}
	if err := store.Load("b", &value); err != nil || value.Value != "two" {
		t.Fatalf("value=%#v err=%v", value, err)
	}
	for _, path := range []string{root, filepath.Join(root, "a.json")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o700)
		if !info.IsDir() {
			want = 0o600
		}
		if info.Mode().Perm() != want {
			t.Errorf("mode=%o want=%o", info.Mode().Perm(), want)
		}
	}
	if err := store.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("../bad", value); err == nil {
		t.Fatal("accepted unsafe ID")
	}
}

func TestQuarantineRetainsInvalidRecordOutsideLiveIDs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "broken.json"), []byte(`{"unknown":true}\n`), 0o600); err != nil {
		t.Fatal(err)
	}
	var value struct {
		Value string `json:"value"`
	}
	if err := store.Load("broken", &value); err == nil {
		t.Fatal("strict load accepted an unknown field")
	}
	quarantined, err := store.Quarantine("broken")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(quarantined); err != nil || string(data) != `{"unknown":true}\n` {
		t.Fatalf("data=%q err=%v", data, err)
	}
	ids, err := store.IDs()
	if err != nil || len(ids) != 0 {
		t.Fatalf("ids=%#v err=%v", ids, err)
	}
	info, err := os.Stat(filepath.Dir(quarantined))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("quarantine mode=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestNewPreloadsRecordsForCacheOnlyReads(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "existing.json")
	if err := os.WriteFile(path, []byte("{\"value\":\"cached\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	var value struct {
		Value string `json:"value"`
	}
	if err := store.Load("existing", &value); err != nil || value.Value != "cached" {
		t.Fatalf("cached value=%#v err=%v", value, err)
	}
	ids, err := store.IDs()
	if err != nil || len(ids) != 1 || ids[0] != "existing" {
		t.Fatalf("cached IDs=%#v err=%v", ids, err)
	}
}

func TestIndependentRecordsAreConcurrencySafe(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "records"))
	if err != nil {
		t.Fatal(err)
	}
	const count = 64
	var wait sync.WaitGroup
	wait.Add(count)
	for index := range count {
		go func() {
			defer wait.Done()
			id := fmt.Sprintf("record-%d", index)
			if err := store.Save(id, map[string]int{"value": index}); err != nil {
				t.Errorf("save %s: %v", id, err)
			}
		}()
	}
	wait.Wait()
	ids, err := store.IDs()
	if err != nil || len(ids) != count {
		t.Fatalf("IDs=%d err=%v", len(ids), err)
	}
}
