package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	maxArtifactBytes  = 512 << 20
	maxExpandedBytes  = 32 << 20
	maxArchiveEntry   = 16 << 20
	maxArchiveDepth   = 4
	maxArchiveEntries = 10000
)

var signatures = []struct {
	name string
	re   *regexp.Regexp
}{
	{"synthetic-sentinel", regexp.MustCompile(`KSG_(?:TEST_SECRET|RUN_SENTINEL)|S1NHX1RFU1RfU0VDUkVU|S1NHX1JVTl9TRU5USU5F`)},
	{"private-key", regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )?PRIVATE KEY-----`)},
	{"bcrypt", regexp.MustCompile(`\$2[abxy]\$[0-9]{2}\$[./A-Za-z0-9]{53}`)},
	{"managed-checksum", regexp.MustCompile(`secretgenerator\.mittwald\.de/managed-data-checksums|managed[-_ ]?checksum[^\r\n]{0,128}[0-9a-fA-F]{64}`)},
	{"password", regexp.MustCompile(`(?i)(?:"password"[[:space:]]*:[[:space:]]*"[^"[:space:]][^"]*"|(?:^|[\r\n])[[:space:]]*password[[:space:]]*[:=][[:space:]]*[^[:space:]<{][^\r\n]*)`)},
}

type unsafeError struct {
	path, category string
}

func (e *unsafeError) Error() string {
	sum := sha256.Sum256([]byte(e.path))
	return fmt.Sprintf("unsafe artifact (category=%s, id=%x)", e.category, sum[:6])
}

type scanBudget struct {
	expanded int64
	entries  int
}

func main() {
	stage := flag.String("stage", "", "copy safe inputs into this new directory")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: check-test-artifacts.sh [--stage DIR] FILE_OR_DIR...")
		os.Exit(2)
	}
	paths := flag.Args()
	if *stage != "" {
		if err := stageAndScan(*stage, paths); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	for _, path := range paths {
		if err := scanPathWithBudget(path, &scanBudget{}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func stageAndScan(stage string, inputs []string) error {
	if _, err := os.Lstat(stage); err == nil {
		return errors.New("artifact stage path already exists")
	} else if !os.IsNotExist(err) {
		return errors.New("cannot inspect artifact stage path")
	}
	parent := filepath.Dir(stage)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return errors.New("cannot create artifact stage parent")
	}
	tmp, err := os.MkdirTemp(parent, ".artifact-stage-")
	if err != nil {
		return errors.New("cannot create artifact stage")
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(tmp)
		}
	}()
	seen := map[string]bool{}
	for _, input := range inputs {
		clean := filepath.Clean(input)
		base := filepath.Base(clean)
		if base == "." || base == string(filepath.Separator) || seen[base] {
			return errors.New("artifact inputs have an invalid or duplicate base name")
		}
		seen[base] = true
		if err := copyPath(clean, filepath.Join(tmp, base)); err != nil {
			return err
		}
	}
	if err := scanPathWithBudget(tmp, &scanBudget{}); err != nil {
		return err
	}
	if err := os.Rename(tmp, stage); err != nil {
		return errors.New("cannot publish artifact stage")
	}
	ok = true
	return nil
}

func copyPath(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return errors.New("cannot inspect artifact path")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return &unsafeError{src, "symbolic-link"}
	}
	if info.IsDir() {
		if !readableDir(info.Mode()) {
			return &unsafeError{src, "unreadable"}
		}
		if err := os.Mkdir(dst, 0o700); err != nil {
			return errors.New("cannot create staged artifact directory")
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return &unsafeError{src, "unreadable"}
		}
		for _, entry := range entries {
			if err := copyPath(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return &unsafeError{src, "non-regular"}
	}
	if info.Mode().Perm()&0o444 == 0 {
		return &unsafeError{src, "unreadable"}
	}
	in, err := os.Open(src)
	if err != nil {
		return &unsafeError{src, "unreadable"}
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return &unsafeError{src, "changed-during-open"}
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("cannot create staged artifact file")
	}
	_, copyErr := io.Copy(out, io.LimitReader(in, maxArtifactBytes+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		return errors.New("cannot copy staged artifact")
	}
	staged, err := os.Stat(dst)
	if err != nil || staged.Size() > maxArtifactBytes {
		return &unsafeError{src, "size-limit"}
	}
	return nil
}

func scanPath(path string) error {
	return scanPathWithBudget(path, &scanBudget{})
}

func scanPathWithBudget(path string, budget *scanBudget) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("cannot inspect artifact path")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return &unsafeError{path, "symbolic-link"}
	}
	if info.IsDir() {
		if !readableDir(info.Mode()) {
			return &unsafeError{path, "unreadable"}
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return &unsafeError{path, "unreadable"}
		}
		for _, entry := range entries {
			if err := scanPathWithBudget(filepath.Join(path, entry.Name()), budget); err != nil {
				return err
			}
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return &unsafeError{path, "non-regular"}
	}
	if info.Mode().Perm()&0o444 == 0 {
		return &unsafeError{path, "unreadable"}
	}
	f, err := os.Open(path)
	if err != nil {
		return &unsafeError{path, "unreadable"}
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return &unsafeError{path, "changed-during-open"}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxArtifactBytes+1))
	if err != nil {
		return &unsafeError{path, "unreadable"}
	}
	if len(data) > maxArtifactBytes {
		return &unsafeError{path, "size-limit"}
	}
	return scanDataBudget(path, data, 0, budget)
}

func scanData(path string, data []byte, depth int) error {
	return scanDataBudget(path, data, depth, &scanBudget{})
}

func scanDataBudget(path string, data []byte, depth int, budget *scanBudget) error {
	for _, signature := range signatures {
		if signature.re.FindIndex(data) != nil {
			return &unsafeError{path, signature.name}
		}
	}
	gzipData := len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
	zipData := looksZip(data)
	tarData := isTar(data)
	lowerPath := strings.ToLower(path)
	archiveSuffix := strings.HasSuffix(lowerPath, ".zip") || strings.HasSuffix(lowerPath, ".tar") || strings.HasSuffix(lowerPath, ".tgz") || strings.HasSuffix(lowerPath, ".tar.gz") || strings.HasSuffix(lowerPath, ".gz")
	if executableData(data) {
		return &unsafeError{path, "executable"}
	}
	if archiveSuffix && !gzipData && !zipData && !tarData {
		return &unsafeError{path, "invalid-archive"}
	}
	if depth >= maxArchiveDepth {
		if gzipData || zipData || tarData {
			return &unsafeError{path, "archive-depth"}
		}
		return nil
	}
	if gzipData {
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return &unsafeError{path, "invalid-gzip"}
		}
		remaining := maxExpandedBytes - budget.expanded
		if remaining <= 0 {
			_ = reader.Close()
			return &unsafeError{path, "archive-limit"}
		}
		unpacked, err := io.ReadAll(io.LimitReader(reader, remaining+1))
		closeErr := reader.Close()
		if err != nil || closeErr != nil || int64(len(unpacked)) > remaining {
			return &unsafeError{path, "archive-limit"}
		}
		budget.expanded += int64(len(unpacked))
		if isTar(unpacked) {
			return scanTar(path, unpacked, depth+1, budget)
		}
		return scanDataBudget(path+"!/gzip", unpacked, depth+1, budget)
	}
	if zipData {
		return scanZip(path, data, depth+1, budget)
	}
	if tarData {
		return scanTar(path, data, depth+1, budget)
	}
	return nil
}

func scanTar(path string, data []byte, depth int, budget *scanBudget) error {
	r := tar.NewReader(bytes.NewReader(data))
	entries, total := 0, int64(0)
	for {
		h, err := r.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return &unsafeError{path, "invalid-tar"}
		}
		entries++
		budget.entries++
		if entries > maxArchiveEntries || budget.entries > maxArchiveEntries {
			return &unsafeError{path, "archive-limit"}
		}
		name, ok := archivePath(h.Name)
		if !ok {
			return &unsafeError{path, "archive-path"}
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			return &unsafeError{path + "!/" + name, "non-regular"}
		}
		total += h.Size
		if h.Size < 0 || h.Size > maxArchiveEntry || total > maxExpandedBytes || budget.expanded+h.Size > maxExpandedBytes {
			return &unsafeError{path, "archive-limit"}
		}
		body, err := io.ReadAll(io.LimitReader(r, maxArtifactBytes+1))
		if err != nil || int64(len(body)) != h.Size {
			return &unsafeError{path, "invalid-tar"}
		}
		budget.expanded += h.Size
		if err := scanDataBudget(path+"!/"+name, body, depth, budget); err != nil {
			return err
		}
	}
}

func scanZip(path string, data []byte, depth int, budget *scanBudget) error {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return &unsafeError{path, "invalid-zip"}
	}
	budget.entries += len(r.File)
	if len(r.File) > maxArchiveEntries || budget.entries > maxArchiveEntries {
		return &unsafeError{path, "archive-limit"}
	}
	total := int64(0)
	for _, f := range r.File {
		name, ok := archivePath(f.Name)
		if !ok {
			return &unsafeError{path, "archive-path"}
		}
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() {
			return &unsafeError{path + "!/" + name, "non-regular"}
		}
		total += int64(f.UncompressedSize64)
		if f.UncompressedSize64 > maxArchiveEntry || total > maxExpandedBytes || budget.expanded+int64(f.UncompressedSize64) > maxExpandedBytes {
			return &unsafeError{path, "archive-limit"}
		}
		rc, err := f.Open()
		if err != nil {
			return &unsafeError{path, "invalid-zip"}
		}
		body, readErr := io.ReadAll(io.LimitReader(rc, maxArtifactBytes+1))
		closeErr := rc.Close()
		if readErr != nil || closeErr != nil || uint64(len(body)) != f.UncompressedSize64 {
			return &unsafeError{path, "invalid-zip"}
		}
		budget.expanded += int64(f.UncompressedSize64)
		if err := scanDataBudget(path+"!/"+name, body, depth, budget); err != nil {
			return err
		}
	}
	return nil
}

func readableDir(mode os.FileMode) bool {
	return mode.Perm()&0o444 != 0 && mode.Perm()&0o111 != 0
}

func archivePath(name string) (string, bool) {
	name = filepath.ToSlash(filepath.Clean(name))
	return name, name != "." && name != ".." && !strings.HasPrefix(name, "../") && !filepath.IsAbs(name)
}

func isTar(data []byte) bool {
	return len(data) >= 512 && bytes.Equal(data[257:262], []byte("ustar"))
}

func looksZip(data []byte) bool {
	start := len(data) - 65557
	if start < 0 {
		start = 0
	}
	return bytes.Contains(data[start:], []byte{'P', 'K', 5, 6})
}

func executableData(data []byte) bool {
	if len(data) >= 4 && (bytes.Equal(data[:4], []byte{0x7f, 'E', 'L', 'F'}) || bytes.Equal(data[:2], []byte{'M', 'Z'})) {
		return true
	}
	if len(data) < 4 {
		return false
	}
	magic := []byte{data[0], data[1], data[2], data[3]}
	for _, candidate := range [][]byte{{0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf}, {0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}} {
		if bytes.Equal(magic, candidate) {
			return true
		}
	}
	return false
}
