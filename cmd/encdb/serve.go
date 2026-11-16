package main

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// web is the explorer's pages, and the snapshot of encodes.parquet they
// load by default.
//
//go:embed web
var web embed.FS

// handler serves the pages, with encodes.parquet from data if it's set.
// Nothing is cached, so a rebuilt table or an edited page shows on reload.
func handler(data string) http.Handler {
	root, err := fs.Sub(web, "web")
	if err != nil {
		panic(err) // the embedded tree always has web/
	}
	files := http.FileServerFS(root)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if data != "" && r.URL.Path == "/encodes.parquet" {
			http.ServeFile(w, r, data)
			return
		}
		files.ServeHTTP(w, r)
	})
}

func serve(ctx context.Context, addr, data string, log *slog.Logger) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: handler(data), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("serving", "url", "http://"+ln.Addr().String()+"/facets.html")
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
