package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
)

// writeTarEntry appends a one-file gzipped tar to w, for building hostile bundles in tests.
func writeTarEntry(w io.Writer, name, body string) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write([]byte(body))
	tw.Close()
	gz.Close()
}
