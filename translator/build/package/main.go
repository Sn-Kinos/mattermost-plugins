package main

import (
	"archive/tar"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

var epoch = time.Unix(0, 0)

func main() {
	root := flag.String("root", "", "bundle root directory")
	out := flag.String("out", "", "output tar.gz file")
	flag.Parse()

	if *root == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "-root and -out are required")
		os.Exit(2)
	}

	file, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer file.Close()

	if err := writeArchive(file, *root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func writeArchive(w io.Writer, root string) error {
	paths, err := archivePaths(root)
	if err != nil {
		return err
	}

	gzipWriter, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return err
	}
	gzipWriter.Header.ModTime = epoch
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)

	for _, path := range paths {
		if err := writeFile(tarWriter, root, path); err != nil {
			return closeArchive(tarWriter, gzipWriter, err)
		}
	}
	return closeArchive(tarWriter, gzipWriter, nil)
}

func archivePaths(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("unsupported archive entry %q", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

func writeFile(tarWriter *tar.Writer, root, archivePath string) error {
	path := filepath.Join(root, filepath.FromSlash(archivePath))
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	mode := int64(0o644)
	if info.Mode()&0o111 != 0 {
		mode = 0o755
	}
	header := &tar.Header{
		Name:    archivePath,
		Mode:    mode,
		Size:    info.Size(),
		ModTime: epoch,
		Uid:     0,
		Gid:     0,
		Uname:   "",
		Gname:   "",
		Format:  tar.FormatUSTAR,
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		return err
	}
	_, err = io.Copy(tarWriter, file)
	return err
}

func closeArchive(tarWriter *tar.Writer, gzipWriter *gzip.Writer, previous error) error {
	if err := tarWriter.Close(); previous == nil && err != nil {
		previous = err
	}
	if err := gzipWriter.Close(); previous == nil && err != nil {
		previous = err
	}
	return previous
}
