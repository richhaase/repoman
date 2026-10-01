package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/richhaase/repoman/internal/syncer"
)

// List reads stored target settings without applying operational defaults.
// An absent or empty file is an empty configuration, ready for registration.
func List(path string) (Config, error) {
	doc, err := readDocument(path)
	return doc.config, err
}

// Register replaces a target's sync settings and appends it in configuration
// order, matching sync-repos add. An existing cleanup level is retained unless
// target supplies one. Registration does not create the target directory.
func Register(path string, target syncer.Target) (Config, error) {
	var err error
	target.Dir, err = Normalize(target.Dir)
	if err != nil {
		return Config{}, err
	}
	if !ownerPattern.MatchString(target.Owner) {
		return Config{}, fmt.Errorf("owner must be a GitHub user or organization login")
	}
	if target.Days < 1 {
		return Config{}, fmt.Errorf("days must be positive")
	}
	noEvents := false
	target.Events = &noEvents
	if _, err = validate(Config{Targets: []syncer.Target{target}}, false); err != nil {
		return Config{}, err
	}
	return update(path, func(c Config) (Config, error) {
		kept := make([]syncer.Target, 0, len(c.Targets)+1)
		for _, existing := range c.Targets {
			dir, _ := Normalize(existing.Dir) // readDocument has already validated it.
			if dir == target.Dir {
				if target.CleanupLevel == "" {
					target.CleanupLevel = existing.CleanupLevel
				}
				continue
			}
			kept = append(kept, existing)
		}
		c.Targets = append(kept, target)
		return c, nil
	})
}

// Remove deletes only a configuration entry. It never removes repositories or
// directories, and removing the last target leaves a valid empty config.
func Remove(path, dir string) (Config, error) {
	normalized, err := Normalize(dir)
	if err != nil {
		return Config{}, err
	}
	// Removing from absent storage must not create configuration directories.
	c, err := List(path)
	if err != nil {
		return Config{}, err
	}
	if len(c.Targets) == 0 {
		return Config{}, fmt.Errorf("no configured target for %q", normalized)
	}
	return update(path, func(c Config) (Config, error) {
		kept := make([]syncer.Target, 0, len(c.Targets))
		found := false
		for _, target := range c.Targets {
			candidate, _ := Normalize(target.Dir)
			if candidate == normalized {
				found = true
				continue
			}
			kept = append(kept, target)
		}
		if !found {
			return Config{}, fmt.Errorf("no configured target for %q", normalized)
		}
		c.Targets = kept
		return c, nil
	})
}

var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

type document struct {
	config Config
	data   []byte
	info   os.FileInfo
}

func readDocument(path string) (document, error) {
	doc := document{config: Config{Targets: []syncer.Target{}}}
	if path == "" {
		return doc, fmt.Errorf("config path is empty")
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return doc, fmt.Errorf("read config %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	doc.info, err = f.Stat()
	if err != nil {
		return doc, fmt.Errorf("stat config %q: %w", path, err)
	}
	if !doc.info.Mode().IsRegular() {
		return doc, fmt.Errorf("config %q is not a regular file", path)
	}
	// Read from this same descriptor so the identity and content describe one file.
	var contents bytes.Buffer
	if _, err = contents.ReadFrom(f); err != nil {
		return doc, fmt.Errorf("read config %q: %w", path, err)
	}
	doc.data = contents.Bytes()
	if len(bytes.TrimSpace(doc.data)) == 0 {
		return doc, nil
	}
	doc.config, err = decode(doc.data)
	if err != nil {
		return doc, err
	}
	if _, err = validate(doc.config, true); err != nil {
		return doc, err
	}
	if doc.config.Targets == nil {
		doc.config.Targets = []syncer.Target{}
	}
	return doc, nil
}

func update(path string, change func(Config) (Config, error)) (Config, error) {
	if path == "" {
		return Config{}, fmt.Errorf("config path is empty")
	}
	// Updating a dotfiles symlink must update its destination, not replace the link.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("resolve config %q: %w", path, err)
	} else if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return Config{}, fmt.Errorf("config %q is a dangling symlink", path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Config{}, fmt.Errorf("create config directory: %w", err)
	}
	// Serialize repoman writers. The final snapshot comparison also catches an
	// external editor changing the file while this update is being prepared.
	lockPath := path + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return Config{}, fmt.Errorf("config update is locked (%s); retry after the other update finishes", lockPath)
	}
	if err != nil {
		return Config{}, fmt.Errorf("lock config: %w", err)
	}
	defer func() { _ = os.Remove(lockPath) }()
	if err = lock.Close(); err != nil {
		return Config{}, fmt.Errorf("close config lock: %w", err)
	}
	doc, err := readDocument(path)
	if err != nil {
		return Config{}, err
	}
	next, err := change(doc.config)
	if err != nil {
		return Config{}, err
	}
	if _, err = validate(next, true); err != nil {
		return Config{}, err
	}
	if err = replace(path, doc, next); err != nil {
		return Config{}, err
	}
	return next, nil
}

func replace(path string, old document, next Config) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".repoman-config-*")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer func() { _ = f.Close() }()
	mode := os.FileMode(0600)
	if old.info != nil {
		mode = old.info.Mode().Perm()
	}
	if err = f.Chmod(mode); err != nil {
		return fmt.Errorf("set config permissions: %w", err)
	}
	encoder := json.NewEncoder(f)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(next); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("sync config: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err = unchanged(path, old); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func unchanged(path string, old document) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("config changed during update; retry")
	}
	current, err := readDocument(path)
	if err != nil {
		return fmt.Errorf("config changed during update; retry: %w", err)
	}
	if old.info == nil && current.info == nil {
		return nil
	}
	if old.info == nil || current.info == nil || !os.SameFile(old.info, current.info) ||
		old.info.Mode() != current.info.Mode() || !bytes.Equal(old.data, current.data) {
		return fmt.Errorf("config changed during update; retry")
	}
	return nil
}
