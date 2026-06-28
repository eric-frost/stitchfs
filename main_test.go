package main

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestRenderSpecAddsDividerPadding(t *testing.T) {
	path1 := writeTestFile(t, "a.md", "alpha")
	path2 := writeTestFile(t, "b.md", "beta")

	rendered, errno := renderSpec([]string{path1, path2})
	if errno != 0 {
		t.Fatalf("renderSpec errno=%v", errno)
	}

	want := string(renderDivider(path1)) + "\n\nalpha\n\n" + string(renderDivider(path2)) + "\n\nbeta"
	if string(rendered) != want {
		t.Fatalf("rendered mismatch\nwant: %q\n got: %q", want, string(rendered))
	}

	files := []backingFile{
		{path: path1, size: int64(len("alpha"))},
		{path: path2, size: int64(len("beta"))},
	}
	if got := renderedSize(files); got != uint64(len(rendered)) {
		t.Fatalf("renderedSize=%d want=%d", got, len(rendered))
	}
}

func TestRenderedSizeFallsBackToReadableBytesForZeroSizedStatFile(t *testing.T) {
	path := writeTestFile(t, "dynamic.md", "")
	origReadFile := readFile
	readFile = func(name string) ([]byte, error) {
		if name == path {
			return []byte("alpha\nbeta"), nil
		}
		return origReadFile(name)
	}
	t.Cleanup(func() {
		readFile = origReadFile
	})

	files, err := parseSpec(url.PathEscape(path))
	if err != nil {
		t.Fatalf("parseSpec error=%v", err)
	}
	rendered, errno := renderSpec(specPaths(files))
	if errno != 0 {
		t.Fatalf("renderSpec errno=%v", errno)
	}
	if got := renderedSize(files); got != uint64(len(rendered)) {
		t.Fatalf("renderedSize=%d want=%d", got, len(rendered))
	}
}

func TestSplitEditedDataPreservesTrailingNewlines(t *testing.T) {
	paths := []string{
		writeTestFile(t, "a.md", "alpha\n"),
		writeTestFile(t, "b.md", "beta\n\n"),
		writeTestFile(t, "c.md", "gamma"),
	}
	want := [][]byte{
		[]byte("alpha\n"),
		[]byte("beta\n\n"),
		[]byte("gamma"),
	}

	rendered, errno := renderSpec(paths)
	if errno != 0 {
		t.Fatalf("renderSpec errno=%v", errno)
	}

	parts, err := splitEditedData(rendered, paths)
	if err != nil {
		t.Fatalf("splitEditedData error=%v", err)
	}

	if len(parts) != len(want) {
		t.Fatalf("parts=%d want=%d", len(parts), len(want))
	}
	for i := range want {
		if !bytes.Equal(parts[i], want[i]) {
			t.Fatalf("part %d mismatch\nwant: %q\n got: %q", i, string(want[i]), string(parts[i]))
		}
	}
}

func TestVirtualFileSetattrTempNoDeadlock(t *testing.T) {
	f := &virtualFile{
		name: "tmp",
		kind: kindTemp,
		mode: 0600,
		uid:  123,
		gid:  456,
		data: []byte("alpha"),
	}
	in := &fuse.SetAttrIn{SetAttrInCommon: fuse.SetAttrInCommon{Valid: fuse.FATTR_SIZE, Size: 3}}
	out := &fuse.AttrOut{}
	done := make(chan syscall.Errno, 1)

	go func() {
		done <- f.Setattr(context.Background(), nil, in, out)
	}()

	select {
	case errno := <-done:
		if errno != 0 {
			t.Fatalf("Setattr errno=%v", errno)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Setattr hung")
	}

	if got := string(f.data); got != "alp" {
		t.Fatalf("data=%q want=%q", got, "alp")
	}
	if out.Size != 3 {
		t.Fatalf("size=%d want=3", out.Size)
	}
	if out.Mode != syscall.S_IFREG|0600 {
		t.Fatalf("mode=%#o want=%#o", out.Mode, syscall.S_IFREG|0600)
	}
	if out.Uid != 123 || out.Gid != 456 {
		t.Fatalf("owner=%d:%d want=123:456", out.Uid, out.Gid)
	}
}

func TestParseSpecExpandsRecursiveGlobAndSkipsEmptyPattern(t *testing.T) {
	dir := t.TempDir()
	path1 := writeTestFileAt(t, dir, "a.md", "alpha")
	path2 := writeTestFileAt(t, dir, "nested/b.md", "beta")
	writeTestFileAt(t, dir, "nested/c.txt", "skip")

	spec := url.PathEscape(filepath.Join(dir, "**", "*.md")) + "," + url.PathEscape(filepath.Join(dir, "missing", "*.md"))
	files, err := parseSpec(spec)
	if err != nil {
		t.Fatalf("parseSpec error=%v", err)
	}

	paths := specPaths(files)
	if len(paths) != 2 {
		t.Fatalf("paths=%d want=2 (%v)", len(paths), paths)
	}
	if paths[0] != path1 || paths[1] != path2 {
		t.Fatalf("paths=%v want=[%q %q]", paths, path1, path2)
	}
}

func TestParseSpecReturnsNoMatchesForEmptyGlobExpansion(t *testing.T) {
	dir := t.TempDir()
	spec := url.PathEscape(filepath.Join(dir, "missing", "*.md"))

	_, err := parseSpec(spec)
	if !errors.Is(err, errNoMatches) {
		t.Fatalf("parseSpec error=%v want=%v", err, errNoMatches)
	}
}

func TestParseSpecSupportsExplicitLiteralPrefix(t *testing.T) {
	path := writeTestFileAt(t, t.TempDir(), "literal[1].md", "alpha")
	spec := url.PathEscape("f:" + path)

	files, err := parseSpec(spec)
	if err != nil {
		t.Fatalf("parseSpec error=%v", err)
	}
	if len(files) != 1 || files[0].path != path {
		t.Fatalf("files=%v want=%q", specPaths(files), path)
	}
}

func TestBuildConcatPathPreservesWildcardSpec(t *testing.T) {
	dir := t.TempDir()
	pattern := filepath.Join(dir, "**", "*.md")

	got, err := buildConcatPath(defaultMountpoint, []string{pattern})
	if err != nil {
		t.Fatalf("buildConcatPath error=%v", err)
	}

	want := filepath.Join(defaultMountpoint, url.PathEscape(pattern))
	if got != want {
		t.Fatalf("path=%q want=%q", got, want)
	}
}

func TestBuildConcatPathSupportsExplicitLiteralPrefix(t *testing.T) {
	path := writeTestFileAt(t, t.TempDir(), "literal[1].md", "alpha")

	got, err := buildConcatPath(defaultMountpoint, []string{"f:" + path})
	if err != nil {
		t.Fatalf("buildConcatPath error=%v", err)
	}

	want := filepath.Join(defaultMountpoint, url.PathEscape("f:"+path))
	if got != want {
		t.Fatalf("path=%q want=%q", got, want)
	}
}

func TestOpenSnapshotsWildcardExpansion(t *testing.T) {
	dir := t.TempDir()
	path1 := writeTestFileAt(t, dir, "a.md", "alpha")
	specName := url.PathEscape(filepath.Join(dir, "*.md"))
	files, err := parseSpec(specName)
	if err != nil {
		t.Fatalf("parseSpec error=%v", err)
	}

	node := &virtualFile{name: specName, kind: kindSpec, paths: specPaths(files)}
	fh, _, errno := node.Open(context.Background(), syscall.O_RDONLY)
	if errno != 0 {
		t.Fatalf("open errno=%v", errno)
	}
	h, ok := fh.(*directHandle)
	if !ok {
		t.Fatalf("handle=%T want *directHandle", fh)
	}
	if len(h.paths) != 1 || h.paths[0] != path1 {
		t.Fatalf("handle paths=%v want=[%q]", h.paths, path1)
	}

	writeTestFileAt(t, dir, "b.md", "beta")
	updated, err := parseSpec(specName)
	if err != nil {
		t.Fatalf("parseSpec updated error=%v", err)
	}
	node.paths = specPaths(updated)

	if len(h.paths) != 1 || h.paths[0] != path1 {
		t.Fatalf("handle paths changed=%v want=[%q]", h.paths, path1)
	}
	if len(node.paths) != 2 {
		t.Fatalf("node paths=%v want 2 entries", node.paths)
	}
}

func writeTestFile(t *testing.T, name string, content string) string {
	t.Helper()
	return writeTestFileAt(t, t.TempDir(), name, content)
}

func writeTestFileAt(t *testing.T, dir string, name string, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
