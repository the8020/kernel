//go:build workflowanalysis

package development

// A deliberately small protocol probe, not a production filesystem. Regular
// files and directories only; no symlink/hardlink/mmap support is claimed.
import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var analysisLE = binary.LittleEndian

type analysisFuse struct {
	mu                 sync.Mutex
	fd                 int
	lower, upper, base string
	paths              map[uint64]string
	ids                map[string]uint64
	next               uint64
	counts             map[uint32]int
}

func (s *analysisFuse) path(rel string) string {
	upper := filepath.Join(s.upper, rel)
	if _, err := os.Lstat(upper); err == nil {
		return upper
	}
	return filepath.Join(s.lower, rel)
}

func (s *analysisFuse) touch(rel string) error {
	upper := filepath.Join(s.upper, rel)
	if _, err := os.Lstat(upper); err == nil {
		return nil
	}
	original, err := os.ReadFile(filepath.Join(s.lower, rel))
	if err != nil {
		return err
	}
	for _, root := range []string{s.base, s.upper} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, rel), original, 0600); err != nil {
			return err
		}
	}
	return nil
}

func (s *analysisFuse) inode(rel string) uint64 {
	if id := s.ids[rel]; id != 0 {
		return id
	}
	s.next++
	s.ids[rel], s.paths[s.next] = s.next, rel
	return s.next
}

func (s *analysisFuse) attr(id uint64) ([]byte, error) {
	var st unix.Stat_t
	if err := unix.Lstat(s.path(s.paths[id]), &st); err != nil {
		return nil, err
	}
	b := make([]byte, 88)
	for i, v := range []uint64{id, uint64(st.Size), uint64(st.Blocks), uint64(st.Atim.Sec), uint64(st.Mtim.Sec), uint64(st.Ctim.Sec)} {
		analysisLE.PutUint64(b[i*8:], v)
	}
	for i, v := range []uint32{uint32(st.Atim.Nsec), uint32(st.Mtim.Nsec), uint32(st.Ctim.Nsec), st.Mode, uint32(st.Nlink), 0, 0, 0, 4096, 0} {
		analysisLE.PutUint32(b[48+i*4:], v)
	}
	return b, nil
}

func (s *analysisFuse) entry(rel string) ([]byte, error) {
	id := s.inode(rel)
	a, err := s.attr(id)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 40)
	analysisLE.PutUint64(b, id)
	analysisLE.PutUint64(b[8:], 1)
	return append(b, a...), nil // zero name/attribute TTL, no stale lower caching
}

func (s *analysisFuse) handle(op uint32, id uint64, p []byte) ([]byte, error) {
	s.counts[op]++
	rel := s.paths[id]
	switch op {
	case 26: // INIT
		b := make([]byte, 64)
		analysisLE.PutUint32(b, 7)
		analysisLE.PutUint32(b[4:], 31)
		analysisLE.PutUint32(b[20:], 4096)
		return b, nil
	case 1: // LOOKUP
		name := strings.TrimRight(string(p), "\x00")
		if strings.Contains(name, "/") || name == ".." {
			return nil, unix.EPERM
		}
		return s.entry(filepath.Join(rel, name))
	case 3: // GETATTR
		a, err := s.attr(id)
		return append(make([]byte, 16), a...), err
	case 4: // SETATTR (size only for the mutation probe)
		valid := analysisLE.Uint32(p)
		if valid&8 != 0 {
			if err := s.touch(rel); err != nil {
				return nil, err
			}
			if err := os.Truncate(filepath.Join(s.upper, rel), int64(analysisLE.Uint64(p[16:]))); err != nil {
				return nil, err
			}
		}
		a, err := s.attr(id)
		return append(make([]byte, 16), a...), err
	case 14, 27: // OPEN / OPENDIR
		if op == 14 && analysisLE.Uint32(p)&3 != 0 {
			if err := s.touch(rel); err != nil {
				return nil, err
			}
		}
		b := make([]byte, 16)
		analysisLE.PutUint64(b, id)
		if op == 14 {
			analysisLE.PutUint32(b[8:], 1)
		} // FOPEN_DIRECT_IO
		return b, nil
	case 15: // READ; resolve on each read to exercise no-restart promotion
		f, err := os.Open(s.path(rel))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		b := make([]byte, analysisLE.Uint32(p[16:]))
		n, _ := f.ReadAt(b, int64(analysisLE.Uint64(p[8:])))
		return b[:n], nil
	case 16: // WRITE
		if err := s.touch(rel); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(filepath.Join(s.upper, rel), os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		n, err := f.WriteAt(p[40:], int64(analysisLE.Uint64(p[8:])))
		b := make([]byte, 8)
		analysisLE.PutUint32(b, uint32(n))
		return b, err
	case 28: // READDIR
		names := map[string]bool{}
		for _, root := range []string{s.lower, s.upper} {
			entries, _ := os.ReadDir(filepath.Join(root, rel))
			for _, e := range entries {
				names[e.Name()] = true
			}
		}
		ordered := make([]string, 0, len(names))
		for name := range names {
			ordered = append(ordered, name)
		}
		sort.Strings(ordered)
		var b []byte
		for index := int(analysisLE.Uint64(p[8:])); index < len(ordered); index++ {
			name := ordered[index]
			child := filepath.Join(rel, name)
			attr, err := s.attr(s.inode(child))
			if err != nil {
				return nil, err
			}
			d := make([]byte, (24+len(name)+7)&^7)
			analysisLE.PutUint64(d, s.inode(child))
			analysisLE.PutUint64(d[8:], uint64(index+1))
			analysisLE.PutUint32(d[16:], uint32(len(name)))
			analysisLE.PutUint32(d[20:], analysisLE.Uint32(attr[60:])>>12)
			copy(d[24:], name)
			if len(b)+len(d) > int(analysisLE.Uint32(p[16:])) {
				break
			}
			b = append(b, d...)
		}
		return b, nil
	case 17: // STATFS
		b := make([]byte, 80)
		analysisLE.PutUint64(b, 1<<20)
		analysisLE.PutUint64(b[8:], 1<<19)
		analysisLE.PutUint64(b[16:], 1<<19)
		analysisLE.PutUint32(b[40:], 4096)
		analysisLE.PutUint32(b[44:], 255)
		analysisLE.PutUint32(b[48:], 4096)
		return b, nil
	case 18, 20, 25, 29, 30, 34, 38: // release/fsync/flush/releasedir/fsyncdir/access/destroy
		return nil, nil
	case 22, 23:
		return nil, unix.ENODATA
	default:
		return nil, unix.ENOSYS
	}
}

func (s *analysisFuse) serve() {
	buf := make([]byte, 1<<20)
	for {
		n, err := unix.Read(s.fd, buf)
		if err != nil || n < 40 {
			return
		}
		op := analysisLE.Uint32(buf[4:])
		unique := analysisLE.Uint64(buf[8:])
		id := analysisLE.Uint64(buf[16:])
		if op == 2 || op == 42 {
			continue
		} // forget has no reply
		s.mu.Lock()
		data, err := s.handle(op, id, buf[40:n])
		s.mu.Unlock()
		h := make([]byte, 16)
		analysisLE.PutUint32(h, uint32(16+len(data)))
		analysisLE.PutUint64(h[8:], unique)
		if err != nil {
			var eno unix.Errno
			if !errors.As(err, &eno) {
				eno = unix.EIO
			}
			analysisLE.PutUint32(h[4:], uint32(-int32(eno)))
		}
		if _, err := unix.Write(s.fd, append(h, data...)); err != nil {
			return
		}
	}
}

func TestWorkflowAnalysisFuse(t *testing.T) {
	m, d, repository := analysisRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs")
	if err := copySystemRoot(ctx, m.config.ImageRoot, rootfs); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rootfs, "workspace/fuse"), 0700); err != nil {
		t.Fatal(err)
	}
	count := 1000
	if value := os.Getenv("WORKFLOW_PROBE_FILES"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 10000 {
			t.Fatal("invalid WORKFLOW_PROBE_FILES")
		}
		count = parsed
	}
	for i := 0; i < count; i++ {
		writeTestFile(t, filepath.Join(repository, "files", fmt.Sprintf("%04d.txt", i)), strings.Repeat("test\n", 32))
	}
	if _, err := gitCommand(ctx, repository, gitIdentity("Probe", "probe@example.test"), "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitCommand(ctx, repository, gitIdentity("Probe", "probe@example.test"), "commit", "-m", "1000 files"); err != nil {
		t.Fatal(err)
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	guest := os.NewFile(uintptr(pair[0]), "fuse-guest")
	defer guest.Close()
	server := &analysisFuse{fd: pair[1], lower: repository, upper: filepath.Join(root, "upper"), base: filepath.Join(root, "base"), paths: map[uint64]string{1: ""}, ids: map[string]uint64{"": 1}, next: 1, counts: map[uint32]int{}}
	for _, path := range []string{server.upper, server.base} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	defer unix.Close(pair[1])
	go server.serve()
	id := "sbx-fuse000001"
	bundle := filepath.Join(d.config.SandboxRoot, id, "bundle")
	if err := d.validate(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d.config.LogRoot, id), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"resolv.conf", "hosts"} {
		if err := copySandboxNetworkFile("/etc/"+name, filepath.Join(bundle, name)); err != nil {
			t.Fatal(err)
		}
	}
	spec := developmentSpec(SandboxStart{SandboxID: id, UserID: "fuseprobe", Packages: m.config.PackagesRoot, RootFS: rootfs, Mounts: []SandboxMount{
		{MountDefinition: MountDefinition{ID: "packages", Target: "/workspace/packages", Behavior: MountSandboxSource, Writable: true}, HostSource: m.config.PackagesRoot},
		{MountDefinition: MountDefinition{ID: "temporary", Target: "/tmp", Behavior: MountEphemeral, Writable: true}},
	}}, bundle)
	spec.Process.Capabilities.Bounding = append(spec.Process.Capabilities.Bounding, "CAP_SYS_ADMIN")
	spec.Process.Capabilities.Effective = append(spec.Process.Capabilities.Effective, "CAP_SYS_ADMIN")
	spec.Process.Capabilities.Permitted = append(spec.Process.Capabilities.Permitted, "CAP_SYS_ADMIN")
	spec.Process.Args = []string{"/bin/sh", "-c", "mount -t fuse fuse /workspace/fuse -o fd=100,user_id=0,group_id=0,rootmode=40000,max_read=4096 && exec sleep 1000000"}
	data, _ := json.Marshal(spec)
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	args := append(d.flags(id, "run"), "--detach", "--pass-fd=3:100", "--bundle="+bundle, id)
	cmd := d.commandContext(ctx, args...)
	cmd.ExtraFiles = []*os.File{guest}
	// A detached sandbox inherits stdout; a capture pipe would keep Run waiting
	// for sandbox shutdown even after the runsc launcher has exited.
	logFile, err := os.Create(filepath.Join(root, "launch.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	err = cmd.Run()
	if err != nil {
		output, _ := os.ReadFile(logFile.Name())
		t.Fatalf("FUSE launch: %v: %s", err, output)
	}
	defer func() { _ = d.Kill(context.Background(), id); _ = d.Delete(context.Background(), id) }()
	fake := Sandbox{SandboxID: id}
	analysisExec(t, d, fake, "for i in $(seq 1 80); do test $(stat -f -c %T /workspace/fuse) = fuse && break; sleep .05; done; cat /workspace/fuse/same.txt")
	t.Log("external FUSE mount works on pinned runsc without host /dev/fuse")
	analysisExec(t, d, fake, "printf 'private\\n' >/workspace/fuse/same.txt")
	base, _ := os.ReadFile(filepath.Join(server.base, "same.txt"))
	upper, _ := os.ReadFile(filepath.Join(server.upper, "same.txt"))
	lower, _ := os.ReadFile(filepath.Join(repository, "same.txt"))
	if string(base) != "base\n" || string(upper) != "private\n" || string(lower) != "base\n" {
		t.Fatalf("first-write capture: base=%q upper=%q lower=%q", base, upper, lower)
	}
	writeTestFile(t, filepath.Join(repository, "untouched.txt"), "live-update\n")
	t.Logf("FUSE live lower/private=%q", analysisExec(t, d, fake, "cat /workspace/fuse/untouched.txt /workspace/fuse/same.txt"))
	// On a successfully published unchanged generation, discard only that upper.
	server.mu.Lock()
	writeTestFile(t, filepath.Join(repository, "same.txt"), "merged\n")
	_ = os.Remove(filepath.Join(server.upper, "same.txt"))
	server.mu.Unlock()
	if got := analysisExec(t, d, fake, "cat /workspace/fuse/same.txt"); got != "merged\n" {
		t.Fatalf("in-place publication=%q", got)
	}
	t.Log("FUSE returns published file after removing one upper, with sandbox still alive")
	elf, err := os.ReadFile(filepath.Join(rootfs, "usr/bin/true"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "elf-probe"), elf, 0755); err != nil {
		t.Fatal(err)
	}
	t.Logf("executable/mmap probe=%q", analysisExec(t, d, fake, "/workspace/packages/the8020/dev-core/elf-probe; echo direct_exit=$?; /workspace/fuse/elf-probe 2>&1; echo fuse_exit=$?; git --git-dir=/workspace/fuse/.git --work-tree=/workspace/fuse status --porcelain 2>&1; echo fuse_git_index_exit=$?"))
	if err := os.Remove(filepath.Join(repository, "elf-probe")); err != nil {
		t.Fatal(err)
	}
	t.Logf("scan shape=%d tracked small files", count)
	for _, mount := range []string{"/workspace/packages/the8020/dev-core", "/workspace/fuse"} {
		command := "GIT_INDEX_FILE=/tmp/probe-index git --git-dir=/workspace/packages/the8020/dev-core/.git --work-tree=" + mount + " add -A"
		for trial := 0; trial < 4; trial++ {
			if trial == 0 {
				analysisExec(t, d, fake, "rm -f /tmp/probe-index")
			}
			started := time.Now()
			analysisExec(t, d, fake, command)
			t.Logf("scan mount=%s trial=%d duration=%s", mount, trial, time.Since(started))
		}
	}
	server.mu.Lock()
	t.Logf("FUSE operation counts=%v", server.counts)
	server.mu.Unlock()
}
