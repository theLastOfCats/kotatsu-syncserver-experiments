package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const compressionThreshold = 1024

var gzipPool = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

func acceptsGzip(header string) bool {
	wildcard := false
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		name := strings.TrimSpace(fields[0])
		if name != "gzip" && name != "*" {
			continue
		}
		quality := 1.0
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(field), "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "q") {
				parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
				if err != nil || parsed < 0 || parsed > 1 {
					quality = 0
				} else {
					quality = parsed
				}
			}
		}
		if name == "gzip" {
			return quality > 0
		}
		wildcard = quality > 0
	}
	return wildcard
}

type compressionWriter struct {
	http.ResponseWriter
	status  int
	started bool
	pending []byte
	gzip    *gzip.Writer
}

func (w *compressionWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *compressionWriter) start(compress bool) error {
	if w.started {
		return nil
	}
	w.started = true
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if compress {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Encoding", "gzip")
		w.gzip = gzipPool.Get().(*gzip.Writer)
		w.gzip.Reset(w.ResponseWriter)
	}
	w.ResponseWriter.WriteHeader(w.status)
	if len(w.pending) != 0 {
		var err error
		if w.gzip != nil {
			_, err = w.gzip.Write(w.pending)
		} else {
			_, err = w.ResponseWriter.Write(w.pending)
		}
		w.pending = nil
		return err
	}
	return nil
}

func (w *compressionWriter) Write(p []byte) (int, error) {
	if !w.started {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		contentType := w.Header().Get("Content-Type")
		eligible := w.status >= 200 && w.status != http.StatusNoContent && w.status != http.StatusNotModified &&
			strings.HasPrefix(strings.ToLower(contentType), "application/json") && w.Header().Get("Content-Encoding") == ""
		if eligible && len(w.pending)+len(p) < compressionThreshold {
			w.pending = append(w.pending, p...)
			return len(p), nil
		}
		if err := w.start(eligible); err != nil {
			return 0, err
		}
	}
	if w.gzip != nil {
		return w.gzip.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *compressionWriter) finish() {
	if !w.started {
		_ = w.start(false)
	}
	if w.gzip != nil {
		_ = w.gzip.Close()
		w.gzip.Reset(io.Discard)
		gzipPool.Put(w.gzip)
	}
}

func withCompression(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Method == http.MethodHead || !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		compressed := &compressionWriter{ResponseWriter: w}
		defer compressed.finish()
		next.ServeHTTP(compressed, r)
	})
}
