package cleanup

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/richhaase/repoman/internal/repository"
)

const (
	maxSnapshotEntries = 100000
	maxSnapshotBytes   = 256 << 20
	maxSnapshotDepth   = 256
)

type contentSnapshot struct {
	digest [sha256.Size]byte
}

type snapshotter struct {
	ctx     context.Context
	root    *os.Root
	hash    hash.Hash
	device  os.FileInfo
	entries int
	bytes   int64
}

// snapshotContent never follows file symlinks, opens special files, or traverses
// a nested Git repository. Root also confines races involving ancestor symlinks
// to this checkout. Limits deliberately fail closed rather than omit content.
func snapshotContent(ctx context.Context, state repository.State) (contentSnapshot, error) {
	if err := checkNestedMounts(state.Path); err != nil {
		return contentSnapshot{}, err
	}
	root, err := os.OpenRoot(state.Path)
	if err != nil {
		return contentSnapshot{}, err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(".")
	if err != nil {
		return contentSnapshot{}, err
	}
	s := snapshotter{ctx: ctx, root: root, hash: sha256.New(), device: info}
	if err := s.walk(".", 0); err != nil {
		return contentSnapshot{}, err
	}
	// The index preserves staged content independent of the working file bytes.
	// Capture it separately: .git is just a routing file in linked worktrees.
	gitRoot, err := os.OpenRoot(state.GitDir)
	if err != nil {
		return contentSnapshot{}, fmt.Errorf("read Git index directory: %w", err)
	}
	defer func() { _ = gitRoot.Close() }()
	s.root = gitRoot
	index, err := gitRoot.Lstat("index")
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprint(s.hash, "index-absent\x00")
	} else if err != nil {
		return contentSnapshot{}, fmt.Errorf("inspect Git index: %w", err)
	} else {
		if !index.Mode().IsRegular() {
			return contentSnapshot{}, errors.New("git index is not a regular file")
		}
		fmt.Fprint(s.hash, "git-index\x00")
		if err := s.file("index", index); err != nil {
			return contentSnapshot{}, err
		}
	}
	var snapshot contentSnapshot
	copy(snapshot.digest[:], s.hash.Sum(nil))
	return snapshot, nil
}

func (snapshot contentSnapshot) validate(ctx context.Context, state repository.State) error {
	fresh, err := snapshotContent(ctx, state)
	if err != nil {
		return fmt.Errorf("revalidate local content for %q: %w", state.Path, err)
	}
	if fresh != snapshot {
		return fmt.Errorf("local content changed in %q during cleanup; rerun preview", state.Path)
	}
	return nil
}

func (s *snapshotter) walk(path string, depth int) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if depth > maxSnapshotDepth {
		return errors.New("content inspection exceeds directory depth limit")
	}
	info, err := s.root.Lstat(path)
	if err != nil {
		return err
	}
	if !sameDevice(s.device, info) {
		return fmt.Errorf("nested filesystem at %q is protected", path)
	}
	if strings.EqualFold(filepath.Base(path), ".git") && !strings.EqualFold(path, ".git") {
		return fmt.Errorf("nested Git repository at %q is protected", path)
	}
	switch {
	case info.IsDir():
		if strings.EqualFold(path, ".git") {
			return errors.New("checkout Git directory is not a linked-worktree routing file")
		}
		return s.directory(path, info, depth)
	case info.Mode().IsRegular():
		return s.file(path, info)
	case info.Mode()&os.ModeSymlink != 0:
		// Hash the link itself, never its target. Git removal unlinks it.
		target, err := s.root.Readlink(path)
		if err != nil {
			return err
		}
		s.metadata(path, info)
		fmt.Fprintf(s.hash, "%q\x00", target)
		return s.unchanged(path, info)
	default:
		return fmt.Errorf("special file at %q is protected", path)
	}
}

func (s *snapshotter) directory(path string, info os.FileInfo, depth int) error {
	file, err := openSnapshotFile(s.root, path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !sameSnapshotFile(info, opened) {
		return fmt.Errorf("directory changed while opening %q", path)
	}
	// Read in bounded batches. os.ReadDir/WalkDir can allocate an unbounded
	// directory listing before an entry-count check has a chance to run.
	var entries []os.DirEntry
	for {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		batch, err := file.ReadDir(256)
		s.entries += len(batch)
		if s.entries > maxSnapshotEntries {
			return fmt.Errorf("content inspection exceeds %d entries", maxSnapshotEntries)
		}
		entries = append(entries, batch...)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	// Bare repositories do not contain .git. Protect even an incomplete-looking
	// bare repository rather than silently discard its refs and object store.
	hasHead, hasObjects := false, false
	for _, entry := range entries {
		hasHead = hasHead || strings.EqualFold(entry.Name(), "HEAD")
		hasObjects = hasObjects || strings.EqualFold(entry.Name(), "objects")
	}
	if hasHead && hasObjects {
		return fmt.Errorf("possible nested bare Git repository at %q is protected", path)
	}
	s.metadata(path, info)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := s.walk(filepath.Join(path, entry.Name()), depth+1); err != nil {
			return err
		}
	}
	return s.unchanged(path, info)
}

func (s *snapshotter) file(path string, info os.FileInfo) error {
	if info.Size() < 0 || info.Size() > maxSnapshotBytes-s.bytes {
		return fmt.Errorf("content inspection exceeds %d bytes", maxSnapshotBytes)
	}
	file, err := openSnapshotFile(s.root, path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !sameSnapshotFile(info, opened) {
		return fmt.Errorf("file changed while opening %q", path)
	}
	s.metadata(path, info)
	remaining := info.Size()
	buffer := make([]byte, 64<<10)
	for remaining > 0 {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		size := min(remaining, int64(len(buffer)))
		n, err := io.ReadFull(file, buffer[:size])
		if err != nil {
			return fmt.Errorf("file changed while reading %q: %w", path, err)
		}
		_, _ = s.hash.Write(buffer[:n])
		remaining -= int64(n)
	}
	s.bytes += info.Size()
	after, err := file.Stat()
	if err != nil || !sameSnapshotFile(info, after) {
		return fmt.Errorf("file changed while reading %q", path)
	}
	return s.unchanged(path, info)
}

func (s *snapshotter) metadata(path string, info os.FileInfo) {
	fmt.Fprintf(s.hash, "%q\x00%d\x00%d\x00%d\x00", path, info.Mode(), info.Size(), info.ModTime().UnixNano())
}

func (s *snapshotter) unchanged(path string, before os.FileInfo) error {
	after, err := s.root.Lstat(path)
	if err != nil || !sameSnapshotFile(before, after) {
		return fmt.Errorf("content changed while inspecting %q", path)
	}
	return nil
}

func sameSnapshotFile(before, after os.FileInfo) bool {
	return before != nil && after != nil && os.SameFile(before, after) && before.Mode() == after.Mode() && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}
