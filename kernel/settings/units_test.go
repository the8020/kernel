package settings

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestBinaryByteUnitsAndDurationsRoundTrip(t *testing.T) {
	for input, want := range map[string]ByteSize{"128 MiB": 128 << 20, "10GiB": 10 << 30, "1kiB": 1024, "1GB": 1000000000, "0B": 0} {
		got, err := parseByteSize(input)
		if err != nil || got != want {
			t.Fatal(input, got, err)
		}
		again, err := parseByteSize(formatByteSize(got))
		if err != nil || again != want {
			t.Fatal("byte round trip", input, again, err)
		}
	}
	d := Definition{Key: "test.age", Type: TypeDuration, Storage: StorageNode, Default: "7d", Environment: "THE8020_TEST_AGE", RuntimeMutable: true, Description: "Test duration."}
	for input, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "1h30m": 90 * time.Minute, "100ms": 100 * time.Millisecond} {
		got, err := normalizeValue(d, input)
		if err != nil || got != Duration(want) {
			t.Fatal(input, got, err)
		}
		again, err := normalizeValue(d, externalValue(got))
		if err != nil || again != got {
			t.Fatal("duration round trip", input, again, err)
		}
	}
	for _, bad := range []any{"0s", "-1h", "99999999999999999999d", "1", "1.5d", int64(1000)} {
		if _, err := normalizeValue(d, bad); err == nil {
			t.Fatal("accepted ambiguous/out-of-range duration", bad)
		}
	}
	files := newPersistencePaths(t)
	m, err := New([]Definition{d}, files, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterApplier([]string{d.Key}, &testApplier{}); err != nil {
		t.Fatal(err)
	}
	info, err := m.Set(context.Background(), d.Key, "3d")
	if err != nil || info.ActiveValue != "3d" || info.RestartPending {
		t.Fatal(info, err)
	}
	data, err := os.ReadFile(files.Node)
	if err != nil || !strings.Contains(string(data), `age = "3d"`) {
		t.Fatal("duration did not persist with unit", err)
	}
	reloaded, err := New([]Definition{d}, files, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := reloaded.Active(d.Key); got != Duration(72*time.Hour) {
		t.Fatal("duration reload", got)
	}
}
