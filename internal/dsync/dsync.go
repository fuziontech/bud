package dsync

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strconv"

	"gitlab.com/mnm/bud/internal/dsync/set"
	"gitlab.com/mnm/bud/pkg/vfs"
)

type skipFunc = func(name string, isDir bool) bool

type option struct {
	Skip skipFunc
}

type Option func(o *option)

// Provide a skip function
// Note: try to skip as high up in the tree as possible.
//
// E.g. if the source filesystem doesn't have bud, it will
// delete bud, even if you're skipping bud/generate.
func WithSkip(skips ...skipFunc) Option {
	return func(o *option) {
		o.Skip = composeSkips(skips)
	}
}

func composeSkips(skips []skipFunc) skipFunc {
	return func(name string, isDir bool) bool {
		for _, skip := range skips {
			if skip(name, isDir) {
				return true
			}
		}
		return false
	}
}

// Dir syncs the source directory from the source filesystem to the target directory
// in the target filesystem
func Dir(sfs fs.FS, sdir string, tfs vfs.ReadWritable, tdir string, options ...Option) error {
	opt := &option{
		Skip: func(string, bool) bool { return false },
	}
	for _, option := range options {
		option(opt)
	}
	ops, err := diff(opt, sfs, sdir, tfs, tdir)
	if err != nil {
		return err
	}
	err = apply(sfs, tfs, ops)
	return err
}

type OpType uint8

func (ot OpType) String() string {
	switch ot {
	case CreateType:
		return "create"
	case UpdateType:
		return "update"
	case DeleteType:
		return "delete"
	default:
		return ""
	}
}

const (
	CreateType OpType = iota + 1
	UpdateType
	DeleteType
)

type Op struct {
	Type OpType
	Path string
	Data []byte
}

func (o Op) String() string {
	return o.Type.String() + ":" + o.Path
}

func diff(opt *option, sfs fs.FS, sdir string, tfs vfs.ReadWritable, tdir string) (ops []Op, err error) {
	sourceEntries, err := fs.ReadDir(sfs, sdir)
	if err != nil {
		return nil, err
	}
	targetEntries, err := fs.ReadDir(tfs, tdir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	sourceSet := set.New(sourceEntries...)
	targetSet := set.New(targetEntries...)
	creates := set.Difference(sourceSet, targetSet)
	deletes := set.Difference(targetSet, sourceSet)
	updates := set.Intersection(sourceSet, targetSet)
	createOps, err := createOps(opt, sfs, sdir, creates.List())
	if err != nil {
		return nil, err
	}
	deleteOps := deleteOps(opt, sdir, deletes.List())
	childOps, err := updateOps(opt, sfs, sdir, tfs, tdir, updates.List())
	if err != nil {
		return nil, err
	}
	ops = append(ops, createOps...)
	ops = append(ops, deleteOps...)
	ops = append(ops, childOps...)
	return ops, nil
}

func createOps(opt *option, sfs fs.FS, dir string, des []fs.DirEntry) (ops []Op, err error) {
	for _, de := range des {
		path := filepath.Join(dir, de.Name())
		if opt.Skip(path, de.IsDir()) {
			continue
		}
		if !de.IsDir() {
			data, err := fs.ReadFile(sfs, path)
			if err != nil {
				// Don't error out on files that don't exist
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return nil, err
			}
			ops = append(ops, Op{CreateType, path, data})
			continue
		}
		des, err := fs.ReadDir(sfs, path)
		if err != nil {
			return nil, err
		}
		createOps, err := createOps(opt, sfs, path, des)
		if err != nil {
			return nil, err
		}
		ops = append(ops, createOps...)
	}
	return ops, nil
}

func deleteOps(opt *option, dir string, des []fs.DirEntry) (ops []Op) {
	for _, de := range des {
		path := filepath.Join(dir, de.Name())
		if opt.Skip(path, de.IsDir()) {
			continue
		}
		ops = append(ops, Op{DeleteType, path, nil})
		continue
	}
	return ops
}

func updateOps(opt *option, sfs fs.FS, sdir string, tfs vfs.ReadWritable, tdir string, des []fs.DirEntry) (ops []Op, err error) {
	for _, de := range des {
		path := filepath.Join(sdir, de.Name())
		if opt.Skip(path, de.IsDir()) {
			continue
		}
		// Recurse directories
		if de.IsDir() {
			childOps, err := diff(opt, sfs, path, tfs, path)
			if err != nil {
				return nil, err
			}
			ops = append(ops, childOps...)
			continue
		}
		// Otherwise, check if the file has changed
		sourceStamp, err := stamp(sfs, path)
		if err != nil {
			return nil, err
		}
		targetStamp, err := stamp(tfs, path)
		if err != nil {
			return nil, err
		}
		// Skip if the source and target are the same
		if sourceStamp == targetStamp {
			continue
		}
		data, err := fs.ReadFile(sfs, path)
		if err != nil {
			// Don't error out on files that don't exist
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		ops = append(ops, Op{UpdateType, path, data})
	}
	return ops, nil
}

func apply(sfs fs.FS, tfs vfs.ReadWritable, ops []Op) error {
	for _, op := range ops {
		switch op.Type {
		case CreateType:
			dir := filepath.Dir(op.Path)
			if err := tfs.MkdirAll(dir, 0755); err != nil {
				return err
			}
			if err := tfs.WriteFile(op.Path, op.Data, 0644); err != nil {
				return err
			}
		case UpdateType:
			if err := tfs.WriteFile(op.Path, op.Data, 0644); err != nil {
				return err
			}
		case DeleteType:
			if err := tfs.RemoveAll(op.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

// Stamp the path, returning "" if the file doesn't exist.
// Uses the modtime and size to determine if a file has changed.
func stamp(fsys fs.FS, path string) (stamp string, err error) {
	stat, err := fs.Stat(fsys, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "-1:-1", nil
		}
		return "", err
	}
	mtime := stat.ModTime().UnixNano()
	size := stat.Size()
	stamp = strconv.Itoa(int(size)) + ":" + strconv.Itoa(int(mtime))
	return stamp, nil
}