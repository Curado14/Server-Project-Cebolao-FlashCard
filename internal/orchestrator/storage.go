package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func ensureRoots(root string) error {
	for _, p := range []string{"db", "incoming/.tmp", "originals", "processed", "quarantine", "exports", "backups"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0700); err != nil {
			return err
		}
	}
	return nil
}
func safePart(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range v {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
func within(root, p string) bool {
	r, _ := filepath.Abs(root)
	q, _ := filepath.Abs(p)
	rel, err := filepath.Rel(r, q)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
func writeBlob(root, id string, data io.Reader, limit int64) (string, int64, string, error) {
	part := filepath.Join(root, "incoming", ".tmp", id+".part")
	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyN(io.MultiWriter(f, h), data, limit+1)
	if err != nil && !errors.Is(err, io.EOF) {
		os.Remove(part)
		return "", 0, "", err
	}
	if n > limit {
		os.Remove(part)
		return "", 0, "", errors.New("limit")
	}
	if err = f.Sync(); err != nil {
		os.Remove(part)
		return "", 0, "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return part, n, sum, nil
}
func durableOriginal(root, event, id string, part string) (string, error) {
	day := time.Now().UTC().Format("2006-01-02")
	dir := filepath.Join(root, "originals", safePart(event), day)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	dst := filepath.Join(dir, id+".jpg")
	if !within(root, dst) {
		return "", fmt.Errorf("path outside root")
	}
	if err := os.Rename(part, dst); err != nil {
		return "", err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return dst, nil
}
