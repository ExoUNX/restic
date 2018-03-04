package archiver

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/restic/restic/internal/checker"
	"github.com/restic/restic/internal/fs"
	"github.com/restic/restic/internal/repository"
	"github.com/restic/restic/internal/restic"
	restictest "github.com/restic/restic/internal/test"
)

func prepareTempdirRepoSrc(t testing.TB, src TestDir) (tempdir string, repo restic.Repository, cleanup func()) {
	tempdir, removeTempdir := restictest.TempDir(t)
	repo, removeRepository := repository.TestRepository(t)

	TestCreateFiles(t, tempdir, src)

	cleanup = func() {
		removeRepository()
		removeTempdir()
	}

	return tempdir, repo, cleanup
}

func TestNewArchiverSaveFile(t *testing.T) {
	var tests = []TestFile{
		TestFile{Content: ""},
		TestFile{Content: "foo"},
		TestFile{Content: string(restictest.Random(23, 12*1024*1024+1287898))},
		TestFile{Content: string(restictest.Random(65, 45*1024*1024+8273413))},
	}

	for _, testfile := range tests {
		t.Run("", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			tempdir, repo, cleanup := prepareTempdirRepoSrc(t, TestDir{"file": testfile})
			defer cleanup()

			arch := NewArchiver{
				Repo: repo,
				Select: func(string, os.FileInfo) bool {
					return true
				},
				FS: fs.Local{},
			}

			err := arch.Valid()
			if err != nil {
				t.Fatal(err)
			}

			node, err := arch.SaveFile(ctx, filepath.Join(tempdir, "file"))
			if err != nil {
				t.Fatal(err)
			}

			err = repo.Flush(ctx)
			if err != nil {
				t.Fatal(err)
			}

			err = repo.SaveIndex(ctx)
			if err != nil {
				t.Fatal(err)
			}

			TestEnsureFileContent(ctx, t, repo, "file", node, testfile)
		})
	}
}

func save(t testing.TB, filename string, data []byte) {
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}

	_, err = f.Write(data)
	if err != nil {
		t.Fatal(err)
	}

	err = f.Sync()
	if err != nil {
		t.Fatal(err)
	}

	err = f.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func lstat(t testing.TB, name string) os.FileInfo {
	fi, err := os.Lstat(name)
	if err != nil {
		t.Fatal(err)
	}

	return fi
}

func setTimestamp(t testing.TB, filename string, atime, mtime time.Time) {
	var utimes = [...]syscall.Timespec{
		syscall.NsecToTimespec(atime.UnixNano()),
		syscall.NsecToTimespec(mtime.UnixNano()),
	}

	err := syscall.UtimesNano(filename, utimes[:])
	if err != nil {
		t.Fatal(err)
	}
}

func remove(t testing.TB, filename string) {
	err := os.Remove(filename)
	if err != nil {
		t.Fatal(err)
	}
}

func nodeFromFI(t testing.TB, filename string, fi os.FileInfo) *restic.Node {
	node, err := restic.NodeFromFileInfo(filename, fi)
	if err != nil {
		t.Fatal(err)
	}

	return node
}

func TestFileChanged(t *testing.T) {
	var defaultContent = []byte("foobar")

	var tests = []struct {
		Name    string
		Content []byte
		Modify  func(t testing.TB, filename string)
	}{
		{
			Name: "same-content-new-file",
			Modify: func(t testing.TB, filename string) {
				err := os.Remove(filename)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}

				save(t, filename, defaultContent)
			},
		},
		{
			Name: "same-content-new-timestamp",
			Modify: func(t testing.TB, filename string) {
				time.Sleep(50 * time.Millisecond)
				save(t, filename, defaultContent)
			},
		},
		{
			Name: "other-content",
			Modify: func(t testing.TB, filename string) {
				err := os.Remove(filename)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}

				save(t, filename, []byte("xxxxxx"))
			},
		},
		{
			Name: "longer-content",
			Modify: func(t testing.TB, filename string) {
				save(t, filename, []byte("xxxxxxxxxxxxxxxxxxxxxx"))
			},
		},
		{
			Name: "new-file",
			Modify: func(t testing.TB, filename string) {
				remove(t, filename)
				save(t, filename, defaultContent)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			tempdir, cleanup := restictest.TempDir(t)
			defer cleanup()

			filename := filepath.Join(tempdir, "file")
			content := defaultContent
			if test.Content != nil {
				content = test.Content
			}
			save(t, filename, content)

			save(t, filename, []byte("foobar"))
			fiBefore := lstat(t, filename)
			node := nodeFromFI(t, filename, fiBefore)

			if fileChanged(fiBefore, node) {
				t.Fatalf("unchanged file detected as changed")
			}

			test.Modify(t, filename)

			fiAfter := lstat(t, filename)
			if !fileChanged(fiAfter, node) {
				t.Fatalf("modified file detected as unchanged")
			}
		})
	}
}

func TestFilChangedSpecialCases(t *testing.T) {
	tempdir, cleanup := restictest.TempDir(t)
	defer cleanup()

	filename := filepath.Join(tempdir, "file")
	content := []byte("foobar")
	save(t, filename, content)

	t.Run("nil-node", func(t *testing.T) {
		fi := lstat(t, filename)
		if !fileChanged(fi, nil) {
			t.Fatal("nil node detected as unchanged")
		}
	})

	t.Run("type-change", func(t *testing.T) {
		fi := lstat(t, filename)
		node := nodeFromFI(t, filename, fi)
		node.Type = "symlink"
		if !fileChanged(fi, node) {
			t.Fatal("node with changed type detected as unchanged")
		}
	})
}

func TestNewArchiverSaveDir(t *testing.T) {
	const targetNodeName = "targetdir"

	var tests = []struct {
		src    TestDir
		chdir  string
		target string
		want   TestDir
	}{
		{
			src: TestDir{
				"targetfile": TestFile{Content: string(restictest.Random(888, 2*1024*1024+5000))},
			},
			target: ".",
			want: TestDir{
				"targetdir": TestDir{
					"targetfile": TestFile{Content: string(restictest.Random(888, 2*1024*1024+5000))},
				},
			},
		},
		{
			src: TestDir{
				"targetdir": TestDir{
					"foo":        TestFile{Content: "foo"},
					"emptyfile":  TestFile{Content: ""},
					"bar":        TestFile{Content: "XXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"},
					"largefile":  TestFile{Content: string(restictest.Random(888, 2*1024*1024+5000))},
					"largerfile": TestFile{Content: string(restictest.Random(234, 5*1024*1024+5000))},
				},
			},
			target: "targetdir",
		},
		{
			src: TestDir{
				"foo":       TestFile{Content: "foo"},
				"emptyfile": TestFile{Content: ""},
				"bar":       TestFile{Content: "XXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"},
			},
			target: ".",
			want: TestDir{
				"targetdir": TestDir{
					"foo":       TestFile{Content: "foo"},
					"emptyfile": TestFile{Content: ""},
					"bar":       TestFile{Content: "XXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"},
				},
			},
		},
		{
			src: TestDir{
				"foo": TestDir{
					"subdir": TestDir{
						"x": TestFile{Content: "xxx"},
						"y": TestFile{Content: "yyyyyyyyyyyyyyyy"},
						"z": TestFile{Content: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
					},
					"file": TestFile{Content: "just a test"},
				},
			},
			chdir:  "foo/subdir",
			target: "../../",
			want: TestDir{
				"targetdir": TestDir{
					"foo": TestDir{
						"subdir": TestDir{
							"x": TestFile{Content: "xxx"},
							"y": TestFile{Content: "yyyyyyyyyyyyyyyy"},
							"z": TestFile{Content: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
						},
						"file": TestFile{Content: "just a test"},
					},
				},
			},
		},
		{
			src: TestDir{
				"foo": TestDir{
					"file":  TestFile{Content: "just a test"},
					"file2": TestFile{Content: "again"},
				},
			},
			target: "./foo",
			want: TestDir{
				"targetdir": TestDir{
					"file":  TestFile{Content: "just a test"},
					"file2": TestFile{Content: "again"},
				},
			},
		},
	}

	for _, test := range tests {
		t.Run("", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			tempdir, repo, cleanup := prepareTempdirRepoSrc(t, test.src)
			defer cleanup()

			arch := NewArchiver{
				Repo: repo,
				Select: func(string, os.FileInfo) bool {
					return true
				},
				FS: fs.Local{},
			}

			err := arch.Valid()
			if err != nil {
				t.Fatal(err)
			}

			chdir := tempdir
			if test.chdir != "" {
				chdir = filepath.Join(chdir, test.chdir)
			}

			back := fs.TestChdir(t, chdir)
			defer back()

			fi, err := fs.Lstat(test.target)
			if err != nil {
				t.Fatal(err)
			}

			node, err := arch.SaveDir(ctx, "/", fi, test.target, nil)
			if err != nil {
				t.Fatal(err)
			}

			node.Name = targetNodeName
			tree := &restic.Tree{Nodes: []*restic.Node{node}}
			treeID, err := repo.SaveTree(ctx, tree)
			if err != nil {
				t.Fatal(err)
			}

			err = repo.Flush(ctx)
			if err != nil {
				t.Fatal(err)
			}

			err = repo.SaveIndex(ctx)
			if err != nil {
				t.Fatal(err)
			}

			want := test.want
			if want == nil {
				want = test.src
			}
			TestEnsureTree(ctx, t, "/", repo, treeID, want)
		})
	}
}

func TestNewArchiverSnapshot(t *testing.T) {
	var tests = []struct {
		name    string
		src     TestDir
		want    TestDir
		chdir   string
		targets []string
	}{
		{
			name: "single-file",
			src: TestDir{
				"foo": TestFile{Content: "foo"},
			},
			targets: []string{"foo"},
		},
		{
			name: "file-current-dir",
			src: TestDir{
				"foo": TestFile{Content: "foo"},
			},
			targets: []string{"./foo"},
		},
		{
			name: "dir",
			src: TestDir{
				"target": TestDir{
					"foo": TestFile{Content: "foo"},
				},
			},
			targets: []string{"target"},
		},
		{
			name: "dir-current-dir",
			src: TestDir{
				"target": TestDir{
					"foo": TestFile{Content: "foo"},
				},
			},
			targets: []string{"./target"},
		},
		{
			name: "content-dir-current-dir",
			src: TestDir{
				"target": TestDir{
					"foo": TestFile{Content: "foo"},
				},
			},
			targets: []string{"./target/."},
		},
		{
			name: "current-dir",
			src: TestDir{
				"target": TestDir{
					"foo": TestFile{Content: "foo"},
				},
			},
			targets: []string{"."},
		},
		{
			name: "subdir",
			src: TestDir{
				"subdir": TestDir{
					"foo": TestFile{Content: "foo"},
					"subsubdir": TestDir{
						"foo": TestFile{Content: "foo in subsubdir"},
					},
				},
				"other": TestFile{Content: "another file"},
			},
			targets: []string{"subdir"},
			want: TestDir{
				"subdir": TestDir{
					"foo": TestFile{Content: "foo"},
					"subsubdir": TestDir{
						"foo": TestFile{Content: "foo in subsubdir"},
					},
				},
			},
		},
		{
			name: "subsubdir",
			src: TestDir{
				"subdir": TestDir{
					"foo": TestFile{Content: "foo"},
					"subsubdir": TestDir{
						"foo": TestFile{Content: "foo in subsubdir"},
					},
				},
				"other": TestFile{Content: "another file"},
			},
			targets: []string{"subdir/subsubdir"},
			want: TestDir{
				"subdir": TestDir{
					"subsubdir": TestDir{
						"foo": TestFile{Content: "foo in subsubdir"},
					},
				},
			},
		},
		{
			name: "parent-dir",
			src: TestDir{
				"subdir": TestDir{
					"foo": TestFile{Content: "foo"},
				},
				"other": TestFile{Content: "another file"},
			},
			chdir:   "subdir",
			targets: []string{".."},
		},
		{
			name: "parent-parent-dir",
			src: TestDir{
				"subdir": TestDir{
					"foo": TestFile{Content: "foo"},
					"subsubdir": TestDir{
						"empty": TestFile{Content: ""},
					},
				},
				"other": TestFile{Content: "another file"},
			},
			chdir:   "subdir/subsubdir",
			targets: []string{"../.."},
		},
		{
			name: "parent-parent-dir-slash",
			src: TestDir{
				"subdir": TestDir{
					"subsubdir": TestDir{
						"foo": TestFile{Content: "foo"},
					},
				},
				"other": TestFile{Content: "another file"},
			},
			chdir:   "subdir/subsubdir",
			targets: []string{"../../"},
			want: TestDir{
				"subdir": TestDir{
					"subsubdir": TestDir{
						"foo": TestFile{Content: "foo"},
					},
				},
				"other": TestFile{Content: "another file"},
			},
		},
		{
			name: "parent-subdir",
			src: TestDir{
				"subdir": TestDir{
					"foo": TestFile{Content: "foo"},
				},
				"other": TestFile{Content: "another file"},
			},
			chdir:   "subdir",
			targets: []string{"../subdir"},
			want: TestDir{
				"subdir": TestDir{
					"foo": TestFile{Content: "foo"},
				},
			},
		},
		{
			name: "parent-parent-dir-subdir",
			src: TestDir{
				"subdir": TestDir{
					"subsubdir": TestDir{
						"foo": TestFile{Content: "foo"},
					},
				},
				"other": TestFile{Content: "another file"},
			},
			chdir:   "subdir/subsubdir",
			targets: []string{"../../subdir/subsubdir"},
			want: TestDir{
				"subdir": TestDir{
					"subsubdir": TestDir{
						"foo": TestFile{Content: "foo"},
					},
				},
			},
		},
		{
			name: "included-multiple1",
			src: TestDir{
				"subdir": TestDir{
					"subsubdir": TestDir{
						"foo": TestFile{Content: "foo"},
					},
					"other": TestFile{Content: "another file"},
				},
			},
			targets: []string{"subdir", "subdir/subsubdir"},
		},
		{
			name: "included-multiple2",
			src: TestDir{
				"subdir": TestDir{
					"subsubdir": TestDir{
						"foo": TestFile{Content: "foo"},
					},
					"other": TestFile{Content: "another file"},
				},
			},
			targets: []string{"subdir/subsubdir", "subdir"},
		},
		{
			name: "collision",
			src: TestDir{
				"subdir": TestDir{
					"foo": TestFile{Content: "foo in subdir"},
					"subsubdir": TestDir{
						"foo": TestFile{Content: "foo in subsubdir"},
					},
				},
				"foo": TestFile{Content: "another file"},
			},
			chdir:   "subdir",
			targets: []string{".", "../foo"},
			want: TestDir{

				"foo": TestFile{Content: "foo in subdir"},
				"subsubdir": TestDir{
					"foo": TestFile{Content: "foo in subsubdir"},
				},
				"foo-1": TestFile{Content: "another file"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			tempdir, repo, cleanup := prepareTempdirRepoSrc(t, test.src)
			defer cleanup()

			arch := NewArchiver{
				Repo: repo,
				Select: func(string, os.FileInfo) bool {
					return true
				},
				FS: fs.Local{},
			}

			err := arch.Valid()
			if err != nil {
				t.Fatal(err)
			}

			chdir := tempdir
			if test.chdir != "" {
				chdir = filepath.Join(chdir, filepath.FromSlash(test.chdir))
			}

			back := fs.TestChdir(t, chdir)
			defer back()

			var targets []string
			for _, target := range test.targets {
				targets = append(targets, os.ExpandEnv(target))
			}

			t.Logf("targets: %v", targets)
			sn, snapshotID, err := arch.Snapshot(ctx, targets, Options{Time: time.Now()})
			if err != nil {
				t.Fatal(err)
			}

			t.Logf("saved as %v", snapshotID.Str())

			want := test.want
			if want == nil {
				want = test.src
			}
			TestEnsureSnapshot(t, repo, snapshotID, want)

			checker.TestCheckRepo(t, repo)

			// check that the snapshot contains the targets with absolute paths
			for i, target := range sn.Paths {
				atarget, err := filepath.Abs(test.targets[i])
				if err != nil {
					t.Fatal(err)
				}

				if target != atarget {
					t.Errorf("wrong path in snapshot: want %v, got %v", atarget, target)
				}
			}
		})
	}
}

func TestNewArchiverSnapshotSelect(t *testing.T) {
	var tests = []struct {
		name  string
		src   TestDir
		want  TestDir
		selFn SelectFunc
	}{
		{
			name: "include-all",
			src: TestDir{
				"work": TestDir{
					"foo":     TestFile{Content: "foo"},
					"foo.txt": TestFile{Content: "foo text file"},
					"subdir": TestDir{
						"other":   TestFile{Content: "other in subdir"},
						"bar.txt": TestFile{Content: "bar.txt in subdir"},
					},
				},
				"other": TestFile{Content: "another file"},
			},
			selFn: func(item string, fi os.FileInfo) bool {
				return true
			},
		},
		{
			name: "exclude-all",
			src: TestDir{
				"work": TestDir{
					"foo":     TestFile{Content: "foo"},
					"foo.txt": TestFile{Content: "foo text file"},
					"subdir": TestDir{
						"other":   TestFile{Content: "other in subdir"},
						"bar.txt": TestFile{Content: "bar.txt in subdir"},
					},
				},
				"other": TestFile{Content: "another file"},
			},
			selFn: func(item string, fi os.FileInfo) bool {
				return false
			},
			want: TestDir{},
		},
		{
			name: "exclude-txt-files",
			src: TestDir{
				"work": TestDir{
					"foo":     TestFile{Content: "foo"},
					"foo.txt": TestFile{Content: "foo text file"},
					"subdir": TestDir{
						"other":   TestFile{Content: "other in subdir"},
						"bar.txt": TestFile{Content: "bar.txt in subdir"},
					},
				},
				"other": TestFile{Content: "another file"},
			},
			want: TestDir{
				"work": TestDir{
					"foo": TestFile{Content: "foo"},
					"subdir": TestDir{
						"other": TestFile{Content: "other in subdir"},
					},
				},
				"other": TestFile{Content: "another file"},
			},
			selFn: func(item string, fi os.FileInfo) bool {
				if filepath.Ext(item) == ".txt" {
					return false
				}
				return true
			},
		},
		{
			name: "exclude-dir",
			src: TestDir{
				"work": TestDir{
					"foo":     TestFile{Content: "foo"},
					"foo.txt": TestFile{Content: "foo text file"},
					"subdir": TestDir{
						"other":   TestFile{Content: "other in subdir"},
						"bar.txt": TestFile{Content: "bar.txt in subdir"},
					},
				},
				"other": TestFile{Content: "another file"},
			},
			want: TestDir{
				"work": TestDir{
					"foo":     TestFile{Content: "foo"},
					"foo.txt": TestFile{Content: "foo text file"},
				},
				"other": TestFile{Content: "another file"},
			},
			selFn: func(item string, fi os.FileInfo) bool {
				if filepath.Base(item) == "subdir" {
					return false
				}
				return true
			},
		},
		{
			name: "select-absolute-paths",
			src: TestDir{
				"foo": TestFile{Content: "foo"},
			},
			selFn: func(item string, fi os.FileInfo) bool {
				return filepath.IsAbs(item)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			tempdir, repo, cleanup := prepareTempdirRepoSrc(t, test.src)
			defer cleanup()

			arch := NewArchiver{
				Repo:   repo,
				Select: test.selFn,
				FS:     fs.Local{},
			}

			err := arch.Valid()
			if err != nil {
				t.Fatal(err)
			}

			back := fs.TestChdir(t, tempdir)
			defer back()

			targets := []string{"."}
			_, snapshotID, err := arch.Snapshot(ctx, targets, Options{Time: time.Now()})
			if err != nil {
				t.Fatal(err)
			}

			t.Logf("saved as %v", snapshotID.Str())

			want := test.want
			if want == nil {
				want = test.src
			}
			TestEnsureSnapshot(t, repo, snapshotID, want)

			checker.TestCheckRepo(t, repo)
		})
	}
}

// MockFS keeps track which files are read.
type MockFS struct {
	fs.FS

	m         sync.Mutex
	bytesRead map[string]int // tracks bytes read from all opened files
}

func (m *MockFS) Open(name string) (fs.File, error) {
	f, err := m.FS.Open(name)
	if err != nil {
		return f, err
	}

	return MockFile{File: f, fs: m}, nil
}

func (m *MockFS) OpenFile(name string, flag int, perm os.FileMode) (fs.File, error) {
	f, err := m.FS.OpenFile(name, flag, perm)
	if err != nil {
		return f, err
	}

	return MockFile{File: f, fs: m}, nil
}

type MockFile struct {
	fs.File

	fs *MockFS
}

func (f MockFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if n > 0 {
		f.fs.m.Lock()
		f.fs.bytesRead[f.Name()] += n
		f.fs.m.Unlock()
	}
	return n, err
}

func TestNewArchiverParent(t *testing.T) {
	var tests = []struct {
		src  TestDir
		read map[string]int // tracks number of times a file must have been read
	}{
		{
			src: TestDir{
				"targetfile": TestFile{Content: string(restictest.Random(888, 2*1024*1024+5000))},
			},
			read: map[string]int{
				"targetfile": 1,
			},
		},
	}

	for _, test := range tests {
		t.Run("", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			tempdir, repo, cleanup := prepareTempdirRepoSrc(t, test.src)
			defer cleanup()

			testFS := &MockFS{
				FS:        fs.Local{},
				bytesRead: make(map[string]int),
			}

			arch := NewArchiver{
				Repo: repo,
				Select: func(string, os.FileInfo) bool {
					return true
				},
				FS: testFS,
			}

			back := fs.TestChdir(t, tempdir)
			defer back()

			_, firstSnapshotID, err := arch.Snapshot(ctx, []string{"."}, Options{Time: time.Now()})
			if err != nil {
				t.Fatal(err)
			}

			t.Logf("first backup saved as %v", firstSnapshotID.Str())
			t.Logf("testfs: %v", testFS)

			// check that all files have been read exactly once
			TestWalkFiles(t, ".", test.src, func(filename string, item interface{}) error {
				file, ok := item.(TestFile)
				if !ok {
					return nil
				}

				n, ok := testFS.bytesRead[filename]
				if !ok {
					t.Fatalf("file %v was not read at all", filename)
				}

				if n != len(file.Content) {
					t.Fatalf("file %v: read %v bytes, wanted %v bytes", filename, n, len(file.Content))
				}
				return nil
			})

			opts := Options{
				Time:           time.Now(),
				ParentSnapshot: firstSnapshotID,
			}
			_, secondSnapshotID, err := arch.Snapshot(ctx, []string{"."}, opts)
			if err != nil {
				t.Fatal(err)
			}

			// check that all files still been read exactly once
			TestWalkFiles(t, ".", test.src, func(filename string, item interface{}) error {
				file, ok := item.(TestFile)
				if !ok {
					return nil
				}

				n, ok := testFS.bytesRead[filename]
				if !ok {
					t.Fatalf("file %v was not read at all", filename)
				}

				if n != len(file.Content) {
					t.Fatalf("file %v: read %v bytes, wanted %v bytes", filename, n, len(file.Content))
				}
				return nil
			})

			t.Logf("second backup saved as %v", secondSnapshotID.Str())
			t.Logf("testfs: %v", testFS)

			checker.TestCheckRepo(t, repo)
		})
	}
}