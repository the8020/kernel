package logging

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"the8020/kernel/logging/records"
)

func TestReducedProducerCapacityStillRetiresInheritedFIFOs(t *testing.T) {
	first := newTestManager(t, func(c *Config) { c.MaxProducers = 3 })
	waitReady(t, first)
	const applicationID, otherID = "sbx-0123456789", "sbx-abcdefghij"
	application, err := first.RegisterSandbox(context.Background(), applicationID, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	other, err := first.RegisterSandbox(context.Background(), otherID, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	config := first.config
	config.MaxProducers = 1 // Only the kernel: no inherited sandbox can be adopted.
	second, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	waitReady(t, second)
	if second.Status().RawRecoveryError == "" {
		t.Fatal("incomplete inherited adoption was not reported")
	}
	for range 2 {
		if err := second.UnregisterSandbox(context.Background(), applicationID); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{application.Stdout, application.Stderr} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unadopted application endpoint remains: %v", err)
		}
	}
	for _, path := range []string{other.Stdout, other.Stderr} {
		if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			t.Fatalf("other sandbox endpoint changed: %v", err)
		}
	}
	for range 2 {
		if err := second.UnregisterSandbox(context.Background(), otherID); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{other.Stdout, other.Stderr} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unadopted endpoint remains: %v", err)
		}
	}
	if len(second.registrations) != 1 {
		t.Fatal("cleanup allocated an additional live registration")
	}
}

func TestUnadoptedRetirementPreservesForeignFilesAndSymlinks(t *testing.T) {
	for _, scenario := range []string{"file", "endpoint_link", "directory_link"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			m := &Manager{config: Config{Socket: filepath.Join(root, "api", "logs.sock")}}
			binding := records.Binding{ID: "sbx-0123456789"}
			directory := binding.IngressDirectory(m.config.Socket)
			paths := binding.IngressPaths(m.config.Socket)
			if err := os.MkdirAll(filepath.Dir(directory), 0700); err != nil {
				t.Fatal(err)
			}
			if scenario == "directory_link" {
				foreign := filepath.Join(root, "foreign")
				if err := os.Mkdir(foreign, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, directory); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{paths.Stdout, paths.Stderr} {
					if err := unix.Mkfifo(path, 0600); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
				foreign := paths.Stdout
				if scenario == "endpoint_link" {
					foreign = filepath.Join(root, "foreign.txt")
					if err := os.Symlink(foreign, paths.Stdout); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(foreign, []byte("unrelated data"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.retireUnadoptedRaw(binding); err == nil {
				t.Fatal("accepted non-owned raw endpoint")
			}
			if _, err := os.Lstat(paths.Stdout); err != nil {
				t.Fatalf("foreign endpoint removed: %v", err)
			}
			if scenario != "directory_link" {
				if data, err := os.ReadFile(paths.Stdout); err != nil || string(data) != "unrelated data" {
					t.Fatalf("foreign file changed: %v", err)
				}
			} else if _, err := os.Lstat(paths.Stderr); err != nil {
				t.Fatalf("foreign stderr removed: %v", err)
			}
		})
	}
}
