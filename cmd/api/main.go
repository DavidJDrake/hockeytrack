// Command api serves the query API locally, from the real buckets or from
// directory mirrors, for development and for checking a change against
// real data without deploying.
//
//	api -addr :8080 -bucket my-raw-bucket -index-bucket my-index-bucket -site-bucket my-site-bucket
//	api -addr :8080 -dir ./mirror -index-dir ./index -site-dir ./site
//
// With -dir, the directory mirrors the raw bucket's layout (raw/...);
// -index-dir is what cmd/index wrote; -site-dir holds data/schedule.json.
// Flags fall back to $RAW_BUCKET, $INDEX_BUCKET and $SITE_BUCKET.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"hockeytrack/internal/api"
	"hockeytrack/internal/lake"
	"hockeytrack/internal/store"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	bucket := flag.String("bucket", os.Getenv("RAW_BUCKET"), "raw archive bucket (default $RAW_BUCKET)")
	indexBucket := flag.String("index-bucket", os.Getenv("INDEX_BUCKET"), "index bucket (default $INDEX_BUCKET)")
	siteBucket := flag.String("site-bucket", os.Getenv("SITE_BUCKET"), "site bucket holding data/schedule.json (default $SITE_BUCKET)")
	dir := flag.String("dir", "", "read the raw archive from a local mirror instead of S3")
	indexDir := flag.String("index-dir", "", "read the index from a local directory instead of S3")
	siteDir := flag.String("site-dir", "", "read data/schedule.json from a local directory instead of S3")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var deps api.Deps
	deps.Now = time.Now
	deps.Version = "local"
	needS3 := (*dir == "" && *bucket != "") || (*indexDir == "" && *indexBucket != "") || (*siteDir == "" && *siteBucket != "")
	var s3c *s3.Client
	if needS3 {
		awsCfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			slog.Error("aws config", "err", err)
			os.Exit(1)
		}
		s3c = s3.NewFromConfig(awsCfg)
	}
	pick := func(d, b string) lake.Source {
		switch {
		case d != "":
			return store.DirArchive{Root: d}
		case b != "":
			return store.NewS3Archive(s3c, b)
		}
		return nil
	}
	deps.Raw = pick(*dir, *bucket)
	deps.Index = pick(*indexDir, *indexBucket)
	deps.Site = pick(*siteDir, *siteBucket)
	if deps.Raw == nil || deps.Index == nil {
		fmt.Fprintln(os.Stderr, "api: the raw archive (-bucket or -dir) and the index (-index-bucket or -index-dir) are required")
		os.Exit(2)
	}
	if deps.Site == nil {
		slog.Warn("no site bucket or directory: /v1/schedule and team cards will have no schedule")
	}

	srv := &http.Server{Addr: *addr, Handler: api.New(deps), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("api listening", "addr", "http://"+*addr+"/v1")
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}
