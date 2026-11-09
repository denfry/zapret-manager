// Package safezip extracts a release archive without trusting its contents:
// no path traversal, no absolute paths, no NTFS alternate data streams, and
// bounded file count and sizes (an oversized entry is an error, never a
// silently truncated file).
package safezip

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits bound the resources an archive may consume.
type Limits struct {
	MaxFiles      int   // number of entries
	MaxFileBytes  int64 // uncompressed size of one file
	MaxTotalBytes int64 // uncompressed size of all files
}

// DefaultLimits fit a zapret-discord-youtube release (≈150 files, a few MB)
// with a wide margin.
var DefaultLimits = Limits{MaxFiles: 5000, MaxFileBytes: 200 << 20, MaxTotalBytes: 500 << 20}

// ErrLimit is wrapped by every error caused by Limits.
var ErrLimit = errors.New("архив превышает лимит")

// Extract unpacks zipPath into dest, stripping a common top-level folder
// (zapret-discord-youtube-X/). CRC is verified by archive/zip while reading.
// On error dest may contain a partial extraction; callers extract into a
// temporary folder and rename it on success.
func Extract(zipPath, dest string, lim Limits) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close() //nolint:errcheck // read-only handle
	if len(zr.File) > lim.MaxFiles {
		return fmt.Errorf("%w: %d файлов (максимум %d)", ErrLimit, len(zr.File), lim.MaxFiles)
	}
	// Заявленные размеры проверяем до распаковки, чтобы не писать на диск сотни МБ
	// впустую; настоящие байты всё равно считаются ниже (заголовок может врать).
	var declared uint64
	for _, f := range zr.File {
		declared += f.UncompressedSize64
		if lim.MaxTotalBytes < 0 || declared > uint64(lim.MaxTotalBytes) { // #nosec G115 -- checked non-negative
			return fmt.Errorf("%w: в архиве больше %d байт", ErrLimit, lim.MaxTotalBytes)
		}
	}
	prefix := commonPrefix(zr.File)
	var total int64
	for _, f := range zr.File {
		target, err := EntryPath(dest, f.Name, prefix)
		if err != nil {
			return err
		}
		if target == "" {
			continue
		}
		// папка: «/» в конце (стандарт) или «\» (так пишут некоторые архиваторы Windows)
		if f.FileInfo().IsDir() || strings.HasSuffix(f.Name, `\`) {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		n, err := extractFile(f, target, lim.MaxFileBytes)
		if err != nil {
			return err
		}
		total += n
		if total > lim.MaxTotalBytes {
			return fmt.Errorf("%w: распаковано больше %d байт", ErrLimit, lim.MaxTotalBytes)
		}
	}
	return nil
}

// EntryPath maps an archive entry name to a path inside dest. It returns ""
// for the stripped top-level folder itself and an error for names that would
// escape dest or address an alternate data stream / drive.
func EntryPath(dest, name, prefix string) (string, error) {
	n := strings.TrimPrefix(strings.ReplaceAll(name, `\`, "/"), prefix)
	n = strings.Trim(n, "/")
	if n == "" {
		return "", nil
	}
	if strings.ContainsAny(n, ":\x00") {
		return "", fmt.Errorf("подозрительный путь в архиве: %q", name)
	}
	for part := range strings.SplitSeq(n, "/") {
		// ".." escapes; Win32 also silently strips trailing dots and spaces
		// from a component ("a." is "a", "...." is the parent folder itself).
		if part == ".." || (part != "." && strings.TrimRight(part, ". ") != part) {
			return "", fmt.Errorf("подозрительный путь в архиве: %q", name)
		}
	}
	// IsLocal also rejects reserved device names (NUL, COM1, …) on Windows.
	if !filepath.IsLocal(filepath.FromSlash(n)) {
		return "", fmt.Errorf("подозрительный путь в архиве: %q", name)
	}
	root := filepath.Clean(dest)
	target := filepath.Join(root, filepath.FromSlash(path.Clean(n)))
	rootPrefix := root
	if !strings.HasSuffix(rootPrefix, string(os.PathSeparator)) { // C:\ уже кончается разделителем
		rootPrefix += string(os.PathSeparator)
	}
	if !strings.HasPrefix(strings.ToLower(target), strings.ToLower(rootPrefix)) {
		return "", fmt.Errorf("путь вне папки распаковки: %q", name)
	}
	return target, nil
}

func extractFile(f *zip.File, target string, maxBytes int64) (int64, error) {
	if maxBytes < 0 || f.UncompressedSize64 > uint64(maxBytes) { // #nosec G115 -- maxBytes checked non-negative
		return 0, fmt.Errorf("%w: %s — %d байт (максимум %d)", ErrLimit, f.Name, f.UncompressedSize64, maxBytes)
	}
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer rc.Close() //nolint:errcheck // read-only stream
	out, err := os.Create(target)
	if err != nil {
		return 0, err
	}
	// The header size can lie; count real bytes and fail instead of truncating.
	n, err := io.Copy(out, io.LimitReader(rc, maxBytes+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, err
	}
	if n > maxBytes {
		return n, fmt.Errorf("%w: %s больше %d байт", ErrLimit, f.Name, maxBytes)
	}
	return n, nil
}

func commonPrefix(files []*zip.File) string {
	var top string
	for _, f := range files {
		name := strings.TrimPrefix(strings.ReplaceAll(f.Name, `\`, "/"), "/")
		first, _, found := strings.Cut(name, "/")
		if !found {
			return "" // a file in the archive root — nothing to strip
		}
		if top == "" {
			top = first
		} else if top != first {
			return ""
		}
	}
	if top == "" {
		return ""
	}
	return top + "/"
}
