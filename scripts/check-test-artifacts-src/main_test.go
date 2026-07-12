package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSensitivePatterns(t *testing.T) {
	tests := []struct{ name, data string }{
		{"sentinel", "KSG_TEST_SECRET_fixture"},
		{"base64-run-sentinel", "S1NHX1JVTl9TRU5USU5FX2ZpeHR1cmU="},
		{"pem", "-----BEGIN OPENSSH PRIVATE KEY-----"},
		{"bcrypt", "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"},
		{"checksum-key", "secretgenerator.mittwald.de/managed-data-checksums: value"},
		{"checksum-value", "managed checksum = 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"password", "\npassword: plaintext-value\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := scanData("fixture", []byte(tt.data), 0); err == nil {
				t.Fatal("sensitive fixture succeeded")
			}
		})
	}
}

func TestArchiveDepthFailsClosedWithoutEchoingName(t *testing.T) {
	data := []byte("ordinary")
	for range maxArchiveDepth + 1 {
		var body bytes.Buffer
		w := gzip.NewWriter(&body)
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		data = body.Bytes()
	}
	err := scanData("KSG_TEST_SECRET_filename.gz", data, 0)
	if err == nil {
		t.Fatal("nested archive succeeded")
	}
	if strings.Contains(err.Error(), "KSG_TEST_SECRET") {
		t.Fatal("scanner echoed a sensitive filename")
	}
}

func TestBinaryAndDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	data := append([]byte{0, 1, 2}, []byte("KSG_RUN_SENTINEL_binary")...)
	if err := os.WriteFile(filepath.Join(dir, "nested", "artifact.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scanPath(dir); err == nil {
		t.Fatal("binary sentinel in directory succeeded")
	}
}

func TestTarContentAndSymlinkEntry(t *testing.T) {
	var body bytes.Buffer
	w := tar.NewWriter(&body)
	content := []byte("KSG_TEST_SECRET_archive")
	if err := w.WriteHeader(&tar.Header{Name: "nested/log", Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := scanData("fixture.tar", body.Bytes(), 0); err == nil {
		t.Fatal("archive sentinel succeeded")
	}

	body.Reset()
	w = tar.NewWriter(&body)
	if err := w.WriteHeader(&tar.Header{Name: "escape", Linkname: "../../outside", Typeflag: tar.TypeSymlink}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := scanData("fixture.tar", body.Bytes(), 0); err == nil {
		t.Fatal("archive symlink succeeded")
	}
}

func TestFilesystemSymlinkAndUnreadable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "safe")
	if err := os.WriteFile(target, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := scanPath(link); err == nil {
		t.Fatal("symlink succeeded")
	}
	if err := os.Chmod(target, 0); err != nil {
		t.Fatal(err)
	}
	if err := scanPath(target); err == nil {
		t.Fatal("unreadable file succeeded")
	}
}

func TestStagePreservesOnlySafeArtifacts(t *testing.T) {
	dir := t.TempDir()
	safe := filepath.Join(dir, "safe.log")
	if err := os.WriteFile(safe, []byte("ordinary output\x00ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(dir, "staged")
	if err := stageAndScan(stage, []string{safe}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(stage, "safe.log"))
	if err != nil || !bytes.Equal(got, []byte("ordinary output\x00ok")) {
		t.Fatalf("staged file mismatch: %v", err)
	}

	unsafe := filepath.Join(dir, "unsafe.log")
	if err := os.WriteFile(unsafe, []byte("KSG_TEST_SECRET_block"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(dir, "blocked")
	if err := stageAndScan(blocked, []string{unsafe}); err == nil {
		t.Fatal("unsafe stage succeeded")
	}
	if _, err := os.Lstat(blocked); !os.IsNotExist(err) {
		t.Fatal("unsafe stage was retained")
	}
}
