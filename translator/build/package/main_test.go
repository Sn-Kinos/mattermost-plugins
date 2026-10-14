package main

import (
	"bytes"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPackageIsIdenticalAcrossInputMtimes(t *testing.T) {
	first := packageFixture(t, time.Unix(100, 0))
	second := packageFixture(t, time.Unix(900, 0))
	assert.Equal(t, sha256.Sum256(first), sha256.Sum256(second))
}

func packageFixture(t *testing.T, mtime time.Time) []byte {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "server", "dist"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "plugin.json"), []byte("{}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "server", "dist", "plugin-linux-amd64"), []byte("binary"), 0o755))
	require.NoError(t, filepath.Walk(root, func(path string, _ fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, mtime, mtime)
	}))
	var out bytes.Buffer
	require.NoError(t, writeArchive(&out, root))
	return out.Bytes()
}
