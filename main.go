package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	pathpkg "path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

var (
	version           = "dev"
	dividerPrefix     = "<!--stitchfs:"
	dividerSuffix     = "-->"
	defaultMountpoint = "/mnt/stitchfs"
	activeMountpoint  = defaultMountpoint
	traceOps          bool
	readFile          = os.ReadFile
	statFile          = os.Stat
)

type backingFile struct {
	path string
	size int64
	stat syscall.Stat_t
}

type specEntryKind int

const (
	specEntryLiteral specEntryKind = iota
	specEntryGlob
)

type specEntry struct {
	kind  specEntryKind
	value string
}

var (
	errNoMatches      = errors.New("spec matched no backing files")
	errNotRegularFile = errors.New("not a regular file")
)

type fileKind int

const (
	kindSpec fileKind = iota
	kindTemp
)

type concatRoot struct {
	fs.Inode
}

type virtualFile struct {
	fs.Inode

	mu            sync.Mutex
	name          string
	kind          fileKind
	activeWriters map[uint32]*directHandle

	paths []string
	data  []byte
	mode  uint32
	uid   uint32
	gid   uint32
}

type directHandle struct {
	mu        sync.Mutex
	node      *virtualFile
	pid       uint32
	paths     []string
	data      []byte
	writeable bool
	dirty     bool
	flushed   bool
	flushErr  syscall.Errno
}

type pendingFile struct {
	target string
	tmp    string
}

func parseSpec(name string) ([]backingFile, error) {
	entries, err := parseSpecEntries(name)
	if err != nil {
		return nil, err
	}
	return expandSpecEntries(entries)
}

func parseSpecEntries(name string) ([]specEntry, error) {
	parts := strings.Split(name, ",")
	if len(parts) == 0 {
		return nil, errors.New("empty spec")
	}

	entries := make([]specEntry, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, errors.New("empty path entry")
		}

		decoded, err := url.PathUnescape(part)
		if err != nil {
			return nil, fmt.Errorf("invalid escape in %q: %w", part, err)
		}
		if decoded == "" {
			return nil, errors.New("decoded path is empty")
		}

		entry, err := parseSpecEntry(decoded, activeMountpoint)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

func parseSpecEntry(raw string, mountpoint string) (specEntry, error) {
	kind := specEntryLiteral
	value := raw
	switch {
	case strings.HasPrefix(raw, "g:"):
		kind = specEntryGlob
		value = strings.TrimPrefix(raw, "g:")
	case strings.HasPrefix(raw, "f:"):
		value = strings.TrimPrefix(raw, "f:")
	case hasGlobMeta(raw):
		kind = specEntryGlob
	}

	if value == "" {
		return specEntry{}, errors.New("decoded path is empty")
	}

	cleaned := filepath.Clean(value)
	if !filepath.IsAbs(cleaned) {
		return specEntry{}, fmt.Errorf("spec entry must be absolute: %s", value)
	}

	if kind == specEntryGlob {
		normalized, err := normalizeGlobPattern(cleaned, mountpoint)
		if err != nil {
			return specEntry{}, err
		}
		return specEntry{kind: specEntryGlob, value: normalized}, nil
	}

	resolved, err := resolveBackingPath(cleaned, mountpoint)
	if err != nil {
		return specEntry{}, err
	}

	return specEntry{kind: specEntryLiteral, value: resolved}, nil
}

func expandSpecEntries(entries []specEntry) ([]backingFile, error) {
	files := make([]backingFile, 0, len(entries))
	seen := make(map[string]struct{})

	for _, entry := range entries {
		switch entry.kind {
		case specEntryLiteral:
			file, err := statBackingFile(entry.value)
			if err != nil {
				return nil, err
			}
			if _, ok := seen[file.path]; ok {
				continue
			}
			seen[file.path] = struct{}{}
			files = append(files, file)
		case specEntryGlob:
			matches, err := expandGlobPattern(entry.value)
			if err != nil {
				return nil, err
			}
			for _, match := range matches {
				resolved, err := resolveBackingPath(match, activeMountpoint)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					return nil, err
				}

				file, err := statBackingFile(resolved)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					if errors.Is(err, errNotRegularFile) {
						continue
					}
					return nil, err
				}
				if _, ok := seen[file.path]; ok {
					continue
				}
				seen[file.path] = struct{}{}
				files = append(files, file)
			}
		default:
			return nil, fmt.Errorf("unsupported spec entry kind: %d", entry.kind)
		}
	}

	if len(files) == 0 {
		return nil, errNoMatches
	}

	return files, nil
}

func statBackingFile(path string) (backingFile, error) {
	info, err := statFile(path)
	if err != nil {
		return backingFile{}, err
	}
	if !info.Mode().IsRegular() {
		return backingFile{}, fmt.Errorf("%w: %s", errNotRegularFile, path)
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return backingFile{}, fmt.Errorf("missing stat data: %s", path)
	}

	size := info.Size()
	if size == 0 {
		data, err := readFile(path)
		if err != nil {
			return backingFile{}, err
		}
		size = int64(len(data))
	}

	return backingFile{path: path, size: size, stat: *st}, nil
}

func hasGlobMeta(value string) bool {
	return strings.ContainsAny(value, "*?[")
}

func normalizeGlobPattern(pattern string, mountpoint string) (string, error) {
	cleaned := filepath.Clean(pattern)
	if !filepath.IsAbs(cleaned) {
		abs, err := filepath.Abs(cleaned)
		if err != nil {
			return "", err
		}
		cleaned = abs
	}

	root, suffix := globPatternRoot(cleaned)
	resolvedRoot, err := maybeResolvePatternRoot(root, mountpoint)
	if err != nil {
		return "", err
	}
	if resolvedRoot != "" {
		cleaned = joinPatternPath(resolvedRoot, suffix)
	}

	if mountpoint != "" {
		if mountAbs, err := filepath.Abs(mountpoint); err == nil && pathWithin(mountAbs, cleaned) {
			return "", fmt.Errorf("glob pattern resolves inside stitchfs mountpoint: %s", cleaned)
		}
	}

	return cleaned, nil
}

func maybeResolvePatternRoot(root string, mountpoint string) (string, error) {
	if root == "" {
		return "", nil
	}

	if mountpoint != "" {
		if mountAbs, err := filepath.Abs(mountpoint); err == nil && pathWithin(mountAbs, root) {
			return "", fmt.Errorf("glob pattern resolves inside stitchfs mountpoint: %s", root)
		}
	}

	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return root, nil
		}
		return "", err
	}

	if mountpoint != "" {
		if mountAbs, err := filepath.Abs(mountpoint); err == nil && pathWithin(mountAbs, resolved) {
			return "", fmt.Errorf("glob pattern resolves inside stitchfs mountpoint: %s", resolved)
		}
	}

	return resolved, nil
}

func globPatternRoot(pattern string) (string, []string) {
	cleaned := filepath.Clean(pattern)
	parts := strings.Split(filepath.ToSlash(cleaned), "/")
	index := len(parts)
	for i, part := range parts {
		if part != "" && hasGlobMeta(part) {
			index = i
			break
		}
	}

	rootParts := parts[:index]
	suffix := append([]string(nil), parts[index:]...)
	if len(rootParts) == 0 || (len(rootParts) == 1 && rootParts[0] == "") {
		return string(os.PathSeparator), suffix
	}

	root := filepath.FromSlash(strings.Join(rootParts, "/"))
	if root == "" {
		root = string(os.PathSeparator)
	}
	return root, suffix
}

func joinPatternPath(root string, suffix []string) string {
	if len(suffix) == 0 {
		return filepath.Clean(root)
	}
	parts := append([]string{root}, suffix...)
	return filepath.Clean(filepath.Join(parts...))
}

func expandGlobPattern(pattern string) ([]string, error) {
	root, _ := globPatternRoot(pattern)
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, nil
	}

	matches := make([]string, 0)
	err = filepath.WalkDir(root, func(candidate string, d iofs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		matched, err := matchGlobPattern(pattern, candidate)
		if err != nil {
			return err
		}
		if matched {
			matches = append(matches, candidate)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(matches)
	return matches, nil
}

func matchGlobPattern(pattern string, candidate string) (bool, error) {
	patternParts := strings.Split(filepath.ToSlash(filepath.Clean(pattern)), "/")
	candidateParts := strings.Split(filepath.ToSlash(filepath.Clean(candidate)), "/")
	return matchGlobPatternParts(patternParts, candidateParts)
}

func matchGlobPatternParts(pattern []string, candidate []string) (bool, error) {
	if len(pattern) == 0 {
		return len(candidate) == 0, nil
	}

	if pattern[0] == "" {
		if len(candidate) == 0 || candidate[0] != "" {
			return false, nil
		}
		return matchGlobPatternParts(pattern[1:], candidate[1:])
	}

	if pattern[0] == "**" {
		if len(pattern) == 1 {
			return true, nil
		}
		for i := 0; i <= len(candidate); i++ {
			matched, err := matchGlobPatternParts(pattern[1:], candidate[i:])
			if err != nil {
				return false, err
			}
			if matched {
				return true, nil
			}
		}
		return false, nil
	}

	if len(candidate) == 0 {
		return false, nil
	}

	matched, err := pathpkg.Match(pattern[0], candidate[0])
	if err != nil {
		return false, err
	}
	if !matched {
		return false, nil
	}

	return matchGlobPatternParts(pattern[1:], candidate[1:])
}

func specPaths(files []backingFile) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.path)
	}
	return paths
}

func renderSpec(paths []string) ([]byte, syscall.Errno) {
	var out bytes.Buffer

	for i, path := range paths {
		data, err := readFile(path)
		if err != nil {
			return nil, fs.ToErrno(err)
		}
		if i > 0 {
			out.WriteString("\n\n")
		}
		out.Write(renderDivider(path))
		out.WriteString("\n\n")
		out.Write(data)
	}

	return out.Bytes(), 0
}

func renderedSize(files []backingFile) uint64 {
	var total int64
	for i, file := range files {
		total += file.size
		total += int64(len(renderDivider(file.path)) + 2)
		if i > 0 {
			total += 2
		}
	}
	if total < 0 {
		return 0
	}
	return uint64(total)
}

func renderDivider(path string) []byte {
	return []byte(dividerPrefix + path + dividerSuffix)
}

func splitEditedData(data []byte, paths []string) ([][]byte, error) {
	if len(paths) <= 0 {
		return nil, errors.New("no backing files")
	}

	remaining := append([]byte(nil), data...)
	remaining = trimLeadingWhitespace(remaining)

	result := make([][]byte, 0, len(paths))
	for i, path := range paths {
		divider := renderDivider(path)
		if !bytes.HasPrefix(remaining, divider) {
			return nil, fmt.Errorf("expected divider %q", string(divider))
		}
		remaining = remaining[len(divider):]
		remaining = stripLeadingBoundaryBreaks(remaining)

		if i == len(paths)-1 {
			result = append(result, append([]byte(nil), remaining...))
			remaining = nil
			break
		}

		nextDivider := renderDivider(paths[i+1])
		splitAt := bytes.Index(remaining, nextDivider)
		if splitAt < 0 {
			return nil, fmt.Errorf("missing divider %q", string(nextDivider))
		}

		piece := append([]byte(nil), remaining[:splitAt]...)
		piece = stripTrailingBoundaryBreaks(piece)
		result = append(result, piece)
		remaining = remaining[splitAt:]
	}

	if len(remaining) != 0 {
		return nil, errors.New("unexpected trailing data after final divider")
	}

	return result, nil
}

func trimLeadingWhitespace(data []byte) []byte {
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if !unicode.IsSpace(r) {
			return data
		}
		data = data[size:]
	}
	return data
}

func stripLeadingBoundaryBreaks(data []byte) []byte {
	for i := 0; i < 2; i++ {
		switch {
		case bytes.HasPrefix(data, []byte("\r\n")):
			data = data[2:]
		case bytes.HasPrefix(data, []byte("\n")):
			data = data[1:]
		default:
			return data
		}
	}
	return data
}

func stripTrailingBoundaryBreaks(data []byte) []byte {
	for i := 0; i < 2; i++ {
		switch {
		case bytes.HasSuffix(data, []byte("\r\n")):
			data = data[:len(data)-2]
		case bytes.HasSuffix(data, []byte("\n")):
			data = data[:len(data)-1]
		default:
			return data
		}
	}
	return data
}

func commitSegments(paths []string, data []byte) syscall.Errno {
	parts, err := splitEditedData(data, paths)
	if err != nil {
		return syscall.EPERM
	}

	pending := make([]pendingFile, 0, len(paths))
	for i, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			cleanupPending(pending)
			return fs.ToErrno(err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			cleanupPending(pending)
			return syscall.EIO
		}

		dir := filepath.Dir(path)
		tmp, err := os.CreateTemp(dir, ".stitchfs-*")
		if err != nil {
			cleanupPending(pending)
			return fs.ToErrno(err)
		}

		if _, err := tmp.Write(parts[i]); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			cleanupPending(pending)
			return fs.ToErrno(err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			cleanupPending(pending)
			return fs.ToErrno(err)
		}
		if err := os.Chmod(tmp.Name(), info.Mode().Perm()); err != nil {
			os.Remove(tmp.Name())
			cleanupPending(pending)
			return fs.ToErrno(err)
		}
		if os.Getuid() == 0 {
			if err := os.Chown(tmp.Name(), int(st.Uid), int(st.Gid)); err != nil {
				os.Remove(tmp.Name())
				cleanupPending(pending)
				return fs.ToErrno(err)
			}
		}

		pending = append(pending, pendingFile{target: path, tmp: tmp.Name()})
	}

	for i, file := range pending {
		if err := os.Rename(file.tmp, file.target); err != nil {
			for _, leftover := range pending[i:] {
				os.Remove(leftover.tmp)
			}
			return fs.ToErrno(err)
		}
	}

	return 0
}

func cleanupPending(pending []pendingFile) {
	for _, file := range pending {
		_ = os.Remove(file.tmp)
	}
}

func resolveBackingPath(path string, mountpoint string) (string, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		abs, err := filepath.Abs(cleaned)
		if err != nil {
			return "", err
		}
		cleaned = abs
	}

	var mountAbs string
	if mountpoint != "" {
		if abs, err := filepath.Abs(mountpoint); err == nil {
			mountAbs = abs
			if pathWithin(mountAbs, cleaned) {
				log.Printf("rejecting recursive stitchfs backing path: raw=%q resolved=%q mountpoint=%q", path, cleaned, mountAbs)
				return "", fmt.Errorf("backing file resolves inside stitchfs mountpoint: %s", cleaned)
			}
		}
	}

	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		return "", err
	}

	if mountAbs != "" && pathWithin(mountAbs, resolved) {
		log.Printf("rejecting recursive stitchfs backing path: raw=%q resolved=%q mountpoint=%q", path, resolved, mountAbs)
		return "", fmt.Errorf("backing file resolves inside stitchfs mountpoint: %s", resolved)
	}

	return resolved, nil
}

func pathWithin(base string, target string) bool {
	base = filepath.Clean(base)
	target = filepath.Clean(target)
	if base == target {
		return true
	}
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func tracef(format string, args ...any) {
	if traceOps {
		log.Printf(format, args...)
	}
}

func fileModeFromStat(st syscall.Stat_t) uint32 {
	return uint32(st.Mode) & 0777
}

func statResolvedPaths(paths []string) ([]backingFile, error) {
	files := make([]backingFile, 0, len(paths))
	for _, path := range paths {
		file, err := statBackingFile(path)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return nil, errNoMatches
	}
	return files, nil
}

func refreshSpecPaths(name string) ([]backingFile, []string, error) {
	files, err := parseSpec(name)
	if err != nil {
		return nil, nil, err
	}
	paths := specPaths(files)
	return files, paths, nil
}

func (r *concatRoot) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFDIR | 01777
	return 0
}

func (r *concatRoot) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	return fs.NewListDirStream(nil), 0
}

func (r *concatRoot) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	tracef("lookup pid=%d name=%q", callerPID(ctx), name)
	_, paths, err := refreshSpecPaths(name)
	if err != nil {
		return nil, syscall.ENOENT
	}

	if child := r.GetChild(name); child != nil {
		if node, ok := child.Operations().(*virtualFile); ok && node.kind == kindSpec {
			node.mu.Lock()
			node.paths = append([]string(nil), paths...)
			node.mu.Unlock()
		}
		if getter, ok := child.Operations().(fs.NodeGetattrer); ok {
			var attr fuse.AttrOut
			if errno := getter.Getattr(ctx, nil, &attr); errno != 0 {
				return nil, errno
			}
			out.Attr = attr.Attr
		}
		return child, 0
	}

	node := &virtualFile{
		name:  name,
		kind:  kindSpec,
		paths: paths,
	}
	stable := fs.StableAttr{Mode: syscall.S_IFREG, Ino: hashName(name)}
	child := r.NewInode(ctx, node, stable)
	var attr fuse.AttrOut
	if errno := node.Getattr(ctx, nil, &attr); errno != 0 {
		return nil, errno
	}
	out.Attr = attr.Attr
	return child, 0
}

func (r *concatRoot) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	tracef("create pid=%d name=%q flags=%#x mode=%#o", callerPID(ctx), name, flags, mode)
	if _, err := parseSpecEntries(name); err == nil {
		files, specErr := parseSpec(name)
		if specErr != nil {
			return nil, nil, 0, syscall.ENOENT
		}
		child := r.GetChild(name)
		if child == nil {
			node := &virtualFile{name: name, kind: kindSpec, paths: specPaths(files)}
			child = r.NewInode(ctx, node, fs.StableAttr{Mode: syscall.S_IFREG, Ino: hashName(name)})
			if ok := r.AddChild(name, child, false); !ok {
				child = r.GetChild(name)
			}
		}
		node, ok := child.Operations().(*virtualFile)
		if !ok {
			return nil, nil, 0, syscall.EIO
		}
		fh, fuseFlags, errno := node.Open(ctx, flags)
		if errno != 0 {
			return nil, nil, 0, errno
		}
		var attr fuse.AttrOut
		if errno := node.Getattr(ctx, fh, &attr); errno != 0 {
			return nil, nil, 0, errno
		}
		out.Attr = attr.Attr
		return child, fh, fuseFlags, 0
	}

	if r.GetChild(name) != nil {
		return nil, nil, 0, syscall.EEXIST
	}

	uid, gid := callerIDs(ctx)
	node := &virtualFile{
		name: name,
		kind: kindTemp,
		mode: mode & 0777,
		uid:  uid,
		gid:  gid,
	}
	child := r.NewInode(ctx, node, fs.StableAttr{Mode: syscall.S_IFREG, Ino: hashName(name)})
	if ok := r.AddChild(name, child, false); !ok {
		return nil, nil, 0, syscall.EEXIST
	}
	node.applyOpenFlags(flags)
	var attr fuse.AttrOut
	if errno := node.Getattr(ctx, nil, &attr); errno != 0 {
		return nil, nil, 0, errno
	}
	out.Attr = attr.Attr
	return child, nil, 0, 0
}

func (r *concatRoot) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	tracef("rename pid=%d old=%q new=%q flags=%#x", callerPID(ctx), name, newName, flags)
	if flags != 0 {
		return syscall.EINVAL
	}

	parent, ok := newParent.(*concatRoot)
	if !ok || parent != r {
		return syscall.EXDEV
	}

	child := r.GetChild(name)
	if child == nil {
		files, err := parseSpec(name)
		if err != nil {
			return syscall.ENOENT
		}
		node := &virtualFile{name: name, kind: kindSpec, paths: specPaths(files)}
		child = r.NewInode(ctx, node, fs.StableAttr{Mode: syscall.S_IFREG, Ino: hashName(name)})
		if ok := r.AddChild(name, child, false); !ok {
			child = r.GetChild(name)
		}
	}

	node, ok := child.Operations().(*virtualFile)
	if !ok {
		return syscall.EIO
	}

	newEntries, err := parseSpecEntries(newName)
	newIsSpec := err == nil
	var newFiles []backingFile
	if newIsSpec {
		newFiles, err = expandSpecEntries(newEntries)
		if err != nil {
			return syscall.ENOENT
		}
	}

	node.mu.Lock()
	defer node.mu.Unlock()

	switch {
	case node.kind == kindTemp && newIsSpec:
		errno := commitSegments(specPaths(newFiles), node.data)
		if errno != 0 {
			return errno
		}
		node.kind = kindSpec
		node.paths = specPaths(newFiles)
		node.data = nil
		node.name = newName
		node.mode = 0
		node.uid = 0
		node.gid = 0
		return 0
	case node.kind == kindTemp:
		node.name = newName
		return 0
	case node.kind == kindSpec && !newIsSpec:
		if _, errno := node.refreshSpecPathsLocked(); errno != 0 {
			return errno
		}
		content, errno := renderSpec(node.paths)
		if errno != 0 {
			return errno
		}
		uid, gid := callerIDs(ctx)
		node.kind = kindTemp
		node.data = content
		node.paths = nil
		node.mode = 0600
		node.uid = uid
		node.gid = gid
		node.name = newName
		return 0
	case node.kind == kindSpec && newIsSpec:
		if _, errno := node.refreshSpecPathsLocked(); errno != 0 {
			return errno
		}
		content, errno := renderSpec(node.paths)
		if errno != 0 {
			return errno
		}
		errno = commitSegments(specPaths(newFiles), content)
		if errno != 0 {
			return errno
		}
		node.paths = specPaths(newFiles)
		node.name = newName
		return 0
	default:
		return syscall.EIO
	}
}

func (r *concatRoot) Unlink(ctx context.Context, name string) syscall.Errno {
	tracef("unlink pid=%d name=%q", callerPID(ctx), name)
	child := r.GetChild(name)
	if child == nil {
		return syscall.ENOENT
	}
	node, ok := child.Operations().(*virtualFile)
	if !ok {
		return syscall.EIO
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if node.kind != kindTemp {
		return syscall.EPERM
	}
	return 0
}

func (f *virtualFile) applyOpenFlags(flags uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kind != kindTemp {
		return
	}
	if flags&syscall.O_TRUNC != 0 {
		f.data = nil
	}
}

func (f *virtualFile) refreshSpecPathsLocked() ([]backingFile, syscall.Errno) {
	files, err := parseSpec(f.name)
	if err != nil || len(files) == 0 {
		return nil, syscall.ENOENT
	}
	f.paths = specPaths(files)
	return files, 0
}

func (f *virtualFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	tracef("getattr pid=%d name=%q", callerPID(ctx), f.name)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getattrLocked(out)
}

func (f *virtualFile) getattrLocked(out *fuse.AttrOut) syscall.Errno {
	if f.kind == kindTemp {
		out.Mode = syscall.S_IFREG | f.mode
		out.Size = uint64(len(f.data))
		out.Uid = f.uid
		out.Gid = f.gid
		return 0
	}

	files, errno := f.refreshSpecPathsLocked()
	if errno != 0 {
		return errno
	}

	out.Attr.FromStat(&files[0].stat)
	out.Mode = syscall.S_IFREG | fileModeFromStat(files[0].stat)
	out.Size = renderedSize(files)
	return 0
}

func (f *virtualFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	tracef("open pid=%d name=%q flags=%#x", callerPID(ctx), f.name, flags)
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.kind == kindTemp {
		if flags&syscall.O_TRUNC != 0 {
			f.data = nil
		}
		return nil, 0, 0
	}
	if _, errno := f.refreshSpecPathsLocked(); errno != 0 {
		return nil, 0, errno
	}

	data, errno := renderSpec(f.paths)
	if errno != 0 {
		return nil, 0, errno
	}
	if flags&syscall.O_TRUNC != 0 {
		data = nil
	}
	h := &directHandle{
		node:      f,
		pid:       callerPID(ctx),
		paths:     append([]string(nil), f.paths...),
		data:      data,
		writeable: flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_APPEND|syscall.O_TRUNC) != 0,
	}
	if h.writeable {
		if f.activeWriters == nil {
			f.activeWriters = make(map[uint32]*directHandle)
		}
		f.activeWriters[h.pid] = h
	}
	return h, 0, 0
}

func (f *virtualFile) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	tracef("setattr pid=%d name=%q valid=%#x", callerPID(ctx), f.name, in.Valid)
	if fh != nil {
		if setter, ok := fh.(fs.FileSetattrer); ok {
			return setter.Setattr(ctx, in, out)
		}
	}

	f.mu.Lock()
	if f.kind == kindSpec {
		if h := f.activeWriters[callerPID(ctx)]; h != nil {
			f.mu.Unlock()
			return h.Setattr(ctx, in, out)
		}
	}
	f.mu.Unlock()

	f.mu.Lock()
	defer f.mu.Unlock()

	if sz, ok := in.GetSize(); ok {
		if f.kind == kindTemp {
			f.data = resizeBytes(f.data, int(sz))
		} else {
			if _, errno := f.refreshSpecPathsLocked(); errno != 0 {
				return errno
			}
			data, errno := renderSpec(f.paths)
			if errno != 0 {
				return errno
			}
			data = resizeBytes(data, int(sz))
			if errno := commitSegments(f.paths, data); errno != 0 {
				return errno
			}
		}
	}

	return f.getattrLocked(out)
}

func (f *virtualFile) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	tracef("read pid=%d name=%q off=%d size=%d", callerPID(ctx), f.name, off, len(dest))
	if fh != nil {
		if reader, ok := fh.(fs.FileReader); ok {
			return reader.Read(ctx, dest, off)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if off < 0 || len(dest) == 0 {
		return fuse.ReadResultData(nil), 0
	}

	var data []byte
	if f.kind == kindTemp {
		data = f.data
	} else {
		if _, errno := f.refreshSpecPathsLocked(); errno != 0 {
			return nil, errno
		}
		var errno syscall.Errno
		data, errno = renderSpec(f.paths)
		if errno != 0 {
			return nil, errno
		}
	}

	if off >= int64(len(data)) {
		return fuse.ReadResultData(nil), 0
	}
	end := int(off) + len(dest)
	if end > len(data) {
		end = len(data)
	}
	return fuse.ReadResultData(append([]byte(nil), data[off:end]...)), 0
}

func (f *virtualFile) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	tracef("write pid=%d name=%q off=%d size=%d", callerPID(ctx), f.name, off, len(data))
	if fh != nil {
		if writer, ok := fh.(fs.FileWriter); ok {
			return writer.Write(ctx, data, off)
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kind != kindTemp {
		return 0, syscall.EPERM
	}
	f.data = writeAt(f.data, data, off)
	return uint32(len(data)), 0
}

func (f *virtualFile) Fsync(ctx context.Context, fh fs.FileHandle, flags uint32) syscall.Errno {
	tracef("fsync pid=%d name=%q flags=%#x", callerPID(ctx), f.name, flags)
	if fh != nil {
		if fsyncer, ok := fh.(fs.FileFsyncer); ok {
			return fsyncer.Fsync(ctx, flags)
		}
	}

	return 0
}

func (h *directHandle) Getattr(ctx context.Context, out *fuse.AttrOut) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	mode := uint32(0644)
	if len(h.paths) > 0 {
		files, err := statResolvedPaths(h.paths)
		if err == nil && len(files) > 0 {
			out.Attr.FromStat(&files[0].stat)
			mode = fileModeFromStat(files[0].stat)
		}
	}
	out.Mode = syscall.S_IFREG | mode
	out.Size = uint64(len(h.data))
	return 0
}

func (h *directHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if off < 0 || off >= int64(len(h.data)) || len(dest) == 0 {
		return fuse.ReadResultData(nil), 0
	}
	end := int(off) + len(dest)
	if end > len(h.data) {
		end = len(h.data)
	}
	return fuse.ReadResultData(append([]byte(nil), h.data[off:end]...)), 0
}

func (h *directHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.writeable {
		return 0, syscall.EPERM
	}
	h.data = writeAt(h.data, data, off)
	h.dirty = true
	return uint32(len(data)), 0
}

func (h *directHandle) Setattr(ctx context.Context, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sz, ok := in.GetSize(); ok {
		h.data = resizeBytes(h.data, int(sz))
		h.dirty = true
	}
	out.Size = uint64(len(h.data))
	out.Mode = syscall.S_IFREG | 0644
	return 0
}

func (h *directHandle) Flush(ctx context.Context) syscall.Errno {
	tracef("flush pid=%d name=%q dirty=%t", callerPID(ctx), h.node.name, h.dirty)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.flushed {
		return h.flushErr
	}
	h.flushed = true
	if !h.dirty {
		return 0
	}
	h.flushErr = commitSegments(h.paths, h.data)
	if h.flushErr == 0 {
		h.dirty = false
	}
	return h.flushErr
}

func (h *directHandle) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	return h.Flush(ctx)
}

func (h *directHandle) Release(ctx context.Context) syscall.Errno {
	tracef("release pid=%d name=%q", callerPID(ctx), h.node.name)
	h.node.mu.Lock()
	if h.node.activeWriters != nil && h.node.activeWriters[h.pid] == h {
		delete(h.node.activeWriters, h.pid)
	}
	h.node.mu.Unlock()
	return 0
}

func writeAt(base []byte, data []byte, off int64) []byte {
	if off < 0 {
		return base
	}
	end := int(off) + len(data)
	if end > len(base) {
		grown := make([]byte, end)
		copy(grown, base)
		base = grown
	}
	copy(base[int(off):], data)
	return base
}

func resizeBytes(data []byte, size int) []byte {
	if size < 0 {
		size = 0
	}
	if size <= len(data) {
		return append([]byte(nil), data[:size]...)
	}
	grown := make([]byte, size)
	copy(grown, data)
	return grown
}

func callerIDs(ctx context.Context) (uint32, uint32) {
	caller, ok := fuse.FromContext(ctx)
	if !ok {
		return uint32(os.Getuid()), uint32(os.Getgid())
	}
	return caller.Uid, caller.Gid
}

func callerPID(ctx context.Context) uint32 {
	caller, ok := fuse.FromContext(ctx)
	if !ok {
		return 0
	}
	return caller.Pid
}

func encodeSpecInput(input string, mountpoint string) (string, error) {
	raw := input
	explicitPrefix := ""
	switch {
	case strings.HasPrefix(input, "g:"):
		explicitPrefix = "g:"
		raw = strings.TrimPrefix(input, "g:")
	case strings.HasPrefix(input, "f:"):
		explicitPrefix = "f:"
		raw = strings.TrimPrefix(input, "f:")
	}

	if explicitPrefix == "g:" || (explicitPrefix == "" && hasGlobMeta(raw)) {
		normalized, err := normalizeGlobPattern(raw, mountpoint)
		if err != nil {
			return "", err
		}
		if explicitPrefix == "g:" {
			return url.PathEscape("g:" + normalized), nil
		}
		return url.PathEscape(normalized), nil
	}

	resolved, err := resolveBackingPath(raw, mountpoint)
	if err != nil {
		return "", err
	}
	if _, err := statBackingFile(resolved); err != nil {
		return "", err
	}
	if explicitPrefix == "f:" {
		return url.PathEscape("f:" + resolved), nil
	}
	return url.PathEscape(resolved), nil
}

func buildConcatPath(mountpoint string, inputs []string) (string, error) {
	if len(inputs) == 0 {
		return "", errors.New("no source files provided")
	}
	parts := make([]string, 0, len(inputs))
	for _, input := range inputs {
		encoded, err := encodeSpecInput(input, mountpoint)
		if err != nil {
			return "", err
		}
		parts = append(parts, encoded)
	}
	return filepath.Join(mountpoint, strings.Join(parts, ",")), nil
}

func mountActive(mountpoint string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[4] == mountpoint {
			return true
		}
	}
	return false
}

func ensureMountAvailable(mountpoint string) error {
	if mountActive(mountpoint) {
		return nil
	}

	commands := [][]string{}
	if os.Geteuid() == 0 {
		commands = append(commands, []string{"systemctl", "start", "stitchfs.service"})
	} else {
		commands = append(commands, []string{"sudo", "-n", "systemctl", "start", "stitchfs.service"})
		commands = append(commands, []string{"systemctl", "start", "stitchfs.service"})
	}

	var lastErr error
	for _, argv := range commands {
		cmd := exec.Command(argv[0], argv[1:]...)
		if err := cmd.Run(); err == nil {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if mountActive(mountpoint) {
					return nil
				}
				time.Sleep(100 * time.Millisecond)
			}
			lastErr = fmt.Errorf("stitchfs.service started but %s is not mounted", mountpoint)
			break
		} else {
			lastErr = err
		}
	}

	if mountActive(mountpoint) {
		return nil
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("stitchfs mountpoint %s is not active", mountpoint)
}

func runPathMode(args []string) {
	fs := flag.NewFlagSet("path", flag.ExitOnError)
	mountpoint := fs.String("mountpoint", defaultMountpoint, "stitchfs mountpoint")
	linkPath := fs.String("l", "", "create or update a symlink to the concat path")
	fs.Parse(args)
	if fs.NArg() < 1 {
		log.Fatal("usage: stitchfs [-l LINK] [--mountpoint PATH] FILE [FILE ...]")
	}
	if err := ensureMountAvailable(*mountpoint); err != nil {
		log.Fatal(err)
	}
	path, err := buildConcatPath(*mountpoint, fs.Args())
	if err != nil {
		log.Fatal(err)
	}
	if *linkPath != "" {
		linkAbs, err := filepath.Abs(*linkPath)
		if err != nil {
			log.Fatal(err)
		}
		if info, err := os.Lstat(linkAbs); err == nil {
			if info.Mode()&os.ModeSymlink == 0 {
				log.Fatalf("%s exists and is not a symlink", linkAbs)
			}
			if err := os.Remove(linkAbs); err != nil {
				log.Fatal(err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(linkAbs), 0755); err != nil {
			log.Fatal(err)
		}
		if err := os.Symlink(path, linkAbs); err != nil {
			log.Fatal(err)
		}
		fmt.Println(linkAbs)
		return
	}
	fmt.Println(path)
}

func runMountMode(args []string) {
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	var debug bool
	var trace bool
	var allowOther bool
	var showVersion bool

	flag.BoolVar(&debug, "debug", false, "enable go-fuse debug logging")
	flag.BoolVar(&trace, "trace", false, "enable stitchfs operation logging")
	flag.BoolVar(&allowOther, "allow-other", false, "allow other users to access the mount")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.StringVar(&dividerPrefix, "divider-prefix", dividerPrefix, "marker text before each backing file's path (set both divider flags to suit non-Markdown files)")
	flag.StringVar(&dividerSuffix, "divider-suffix", dividerSuffix, "marker text after each backing file's path")
	flag.CommandLine.Parse(args)

	if showVersion {
		fmt.Println(resolveVersion())
		return
	}

	if flag.NArg() != 1 {
		log.Fatalf("usage: %s mount [-debug] [-allow-other] MOUNTPOINT", os.Args[0])
	}

	mountpoint := flag.Arg(0)
	activeMountpoint = mountpoint
	traceOps = trace
	opts := &fs.Options{
		NullPermissions: true,
		MountOptions: fuse.MountOptions{
			AllowOther: allowOther,
			FsName:     "stitchfs",
			Name:       "stitchfs",
		},
	}
	if allowOther {
		opts.MountOptions.Options = append(opts.MountOptions.Options, "default_permissions")
	}
	opts.Debug = debug

	server, err := fs.Mount(mountpoint, &concatRoot{}, opts)
	if err != nil {
		log.Fatal(err)
	}

	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigc
		server.Unmount()
	}()

	server.Wait()
}

func hashName(name string) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(name); i++ {
		h ^= uint64(name[i])
		h *= 1099511628211
	}
	if h == 0 {
		return 1
	}
	return h
}

// resolveVersion returns the ldflags-injected version, falling back to the
// module version embedded by `go install` so those builds report it too. */
func resolveVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(resolveVersion())
		return
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "mount":
			runMountMode(os.Args[2:])
			return
		case "path":
			runPathMode(os.Args[2:])
			return
		}
	}
	runPathMode(os.Args[1:])
}

var _ = (fs.NodeGetattrer)((*concatRoot)(nil))
var _ = (fs.NodeLookuper)((*concatRoot)(nil))
var _ = (fs.NodeCreater)((*concatRoot)(nil))
var _ = (fs.NodeRenamer)((*concatRoot)(nil))
var _ = (fs.NodeUnlinker)((*concatRoot)(nil))

var _ = (fs.NodeGetattrer)((*virtualFile)(nil))
var _ = (fs.NodeOpener)((*virtualFile)(nil))
var _ = (fs.NodeReader)((*virtualFile)(nil))
var _ = (fs.NodeWriter)((*virtualFile)(nil))
var _ = (fs.NodeSetattrer)((*virtualFile)(nil))
var _ = (fs.NodeFsyncer)((*virtualFile)(nil))

var _ = (fs.FileGetattrer)((*directHandle)(nil))
var _ = (fs.FileReader)((*directHandle)(nil))
var _ = (fs.FileWriter)((*directHandle)(nil))
var _ = (fs.FileSetattrer)((*directHandle)(nil))
var _ = (fs.FileFlusher)((*directHandle)(nil))
var _ = (fs.FileFsyncer)((*directHandle)(nil))
var _ = (fs.FileReleaser)((*directHandle)(nil))
