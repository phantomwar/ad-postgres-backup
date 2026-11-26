package engine

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// ArchiveDirectory streams srcDir into a compressed tar.gz archive at dstPath atomically.
func ArchiveDirectory(srcDir, dstPath string) error {
	info, err := os.Stat(srcDir)
	if err != nil {
		return fmt.Errorf("stat STORAGE_DIR %q: %w", srcDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("STORAGE_DIR %q is not a directory", srcDir)
	}

	if err := os.MkdirAll(filepath.Dir(dstPath), 0o750); err != nil {
		return err
	}
	tmpPath := dstPath + ".part"
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create temp storage archive: %w", err)
	}

	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)

	err = filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		mode := fi.Mode()
		// Skip Unix sockets, FIFOs, and device files safely.
		if !mode.IsRegular() && !mode.IsDir() && mode&os.ModeSymlink == 0 {
			return nil
		}
		var linkTarget string
		if mode&os.ModeSymlink != 0 {
			linkTarget, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, linkTarget)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if mode.IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, f)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		return nil
	})
	if err != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = out.Close()
		return fmt.Errorf("archive directory %q: %w", srcDir, err)
	}

	if err := tw.Close(); err != nil {
		_ = gz.Close()
		_ = out.Close()
		return fmt.Errorf("close tar stream: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = out.Close()
		return fmt.Errorf("close gzip stream: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("sync archive: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		return fmt.Errorf("rename archive: %w", err)
	}
	cleanup = false
	return nil
}
