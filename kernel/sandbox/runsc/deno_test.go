package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDenoConfigUsesCurrentShallowWorkspace(t *testing.T) {
	root := t.TempDir()
	openRoot := func(name string) *os.Root {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		r, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		return r
	}
	fs := &workspaceFS{lower: openRoot("lower"), upper: openRoot("upper"), base: openRoot("base"), deleted: openRoot("deleted"), snapshots: openRoot("snapshots")}
	write := func(tree, name, body string) {
		t.Helper()
		p := filepath.Join(root, tree, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("lower", "team/app/deno.json", "{}")
	write("lower", "team/jsonc/deno.jsonc", "{}")
	write("lower", "team/npm/package.json", "{}")
	write("lower", "team/plain/file.ts", "")
	write("lower", "team/app/deep/nested/deno.json", "invalid")
	write("lower", ".conflicts/attempt/deno.json", "invalid")
	write("lower", ".staging/attempt/deno.json", "invalid")
	write("lower", "node_modules/installed/package.json", "invalid")
	write("lower", "team/node_modules/package.json", "invalid")
	write("upper", "private/new/deno.json", "{}")
	if err := os.Symlink("team", filepath.Join(root, "lower", "alias")); err != nil {
		t.Fatal(err)
	}
	write("lower", "template", `{"workspace":["./packages/*/*"],"imports":{"/p/":"file:///workspace/packages/"},"compilerOptions":{"strict":true}}`)
	template, err := fs.lower.Open("template")
	if err != nil {
		t.Fatal(err)
	}
	defer template.Close()
	config := &denoConfigFS{workspaceFS: fs, template: template}
	check := func(want []string) []byte {
		t.Helper()
		data, s, err := config.snapshot()
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Workspace       []string
			Imports         map[string]string
			CompilerOptions map[string]bool
		}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded.Workspace, want) || decoded.Imports["/p/"] != "file:///workspace/packages/" || !decoded.CompilerOptions["strict"] {
			t.Fatalf("config = %s", data)
		}
		if s.Mode != unix.S_IFREG|0444 || s.Size != uint64(len(data)) {
			t.Fatalf("stat = %+v", s)
		}
		_, again, err := config.snapshot()
		if err != nil || again.Mtime != s.Mtime {
			t.Fatalf("unchanged config changed metadata: %v", err)
		}
		return data
	}
	want := []string{"./packages/private/new", "./packages/team/app", "./packages/team/jsonc", "./packages/team/npm"}
	before := check(want)
	write("deleted", "team/app/deno.json", "")
	check([]string{"./packages/private/new", "./packages/team/jsonc", "./packages/team/npm"})
	write("upper", "team/app/package.json", "{}")
	check(want)
	if err := fs.upper.Rename("private/new", "private/renamed"); err != nil {
		t.Fatal(err)
	}
	want[0] = "./packages/private/renamed"
	after := check(want)
	if bytes.Equal(before, after) {
		t.Fatal("membership did not refresh")
	}
	// Open readers retain their immutable snapshot while later opens see changes.
	reader := &denoConfigOpen{data: bytes.NewReader(before)}
	buffer := make([]byte, len(before))
	if n, err := reader.Read(buffer, 0); err != nil || n != uint64(len(before)) || !bytes.Equal(buffer, before) {
		t.Fatal("snapshot changed")
	}
	write("lower", "template", `{"imports":{"/p/":"file:///workspace/packages/"},"compilerOptions":{"strict":true},"lint":{"rules":{"tags":["recommended"]}}}`)
	if data := check(want); !bytes.Contains(data, []byte(`"lint"`)) {
		t.Fatal("template update lost")
	}
	write("lower", "template", "null")
	if _, _, err := config.snapshot(); err == nil {
		t.Fatal("invalid template accepted")
	}
}
