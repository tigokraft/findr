package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/findr-app/findr/internal/config"
	"github.com/findr-app/findr/internal/domain"
	"github.com/findr-app/findr/internal/downloader"
	qobuzdl "github.com/findr-app/findr/internal/downloader/qobuz"
	slskddl "github.com/findr-app/findr/internal/downloader/slskd"
	"github.com/findr-app/findr/internal/importer"
	"github.com/findr-app/findr/internal/indexer"
	"github.com/findr-app/findr/internal/indexer/deezer"
	qobuzidx "github.com/findr-app/findr/internal/indexer/qobuz"
	"github.com/findr-app/findr/internal/navidrome"
	"github.com/findr-app/findr/internal/orchestrator"
	"github.com/findr-app/findr/internal/storage"
	"github.com/findr-app/findr/internal/verifier"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.CommandLine.Parse(os.Args[2:])

	command := os.Args[1]

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading configuration: %v\n", err)
		os.Exit(1)
	}

	repo, err := storage.NewSQLiteRepository(cfg.Database.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening database: %v\n", err)
		os.Exit(1)
	}
	defer repo.Close()

	// Initialize Indexer (Deezer zero-auth default, or Qobuz)
	var idx indexer.Indexer
	switch cfg.Indexer.DefaultProvider {
	case "qobuz":
		idx = qobuzidx.NewClient(cfg.Indexer.Qobuz.AppID)
	default:
		idx = deezer.NewClient()
	}

	// Initialize Downloader
	var dl downloader.Downloader
	switch cfg.Downloader.Preferred {
	case "slskd":
		dl = slskddl.NewDownloader(slskddl.Config{
			URL:    cfg.Downloader.Slskd.URL,
			APIKey: cfg.Downloader.Slskd.APIKey,
		})
	default:
		dl = qobuzdl.NewDownloader(qobuzdl.Config{
			AppID:         cfg.Downloader.Qobuz.AppID,
			AppSecret:     cfg.Downloader.Qobuz.AppSecret,
			Username:      cfg.Downloader.Qobuz.Username,
			Password:      cfg.Downloader.Qobuz.Password,
			TargetQuality: domain.AudioQuality(cfg.Downloader.Qobuz.TargetQuality),
		})
	}

	// Initialize Verifier & Importer
	vr := verifier.NewVerifier(verifier.Config{
		DurationToleranceSec: cfg.Verifier.DurationToleranceSec,
		MinConfidenceScore:   cfg.Verifier.MinConfidenceScore,
		AllowPartialMatch:    cfg.Verifier.AllowPartialMatch,
	})

	imp := importer.NewImporter(importer.Config{
		MusicPath:    cfg.Library.MusicPath,
		FolderFormat: cfg.Library.FolderFormat,
		FileFormat:   cfg.Library.FileFormat,
	})

	var navi *navidrome.Client
	if cfg.Navidrome.Enabled {
		navi = navidrome.NewClient(navidrome.Config{
			URL:       cfg.Navidrome.URL,
			Username:  cfg.Navidrome.Username,
			Password:  cfg.Navidrome.Password,
			TokenAuth: cfg.Navidrome.TokenAuth,
		})
	}

	orch := orchestrator.NewOrchestrator(cfg, repo, idx, dl, vr, imp, navi)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cancel()
	}()

	switch command {
	case "search":
		handleSearch(ctx, idx)
	case "add":
		handleAdd(ctx, orch)
	case "albums":
		handleListAlbums(ctx, repo, idx)
	case "download":
		handleDownload(ctx, orch)
	case "scan":
		handleScan(ctx, navi)
	case "artists":
		handleListArtists(ctx, repo)
	case "version":
		fmt.Printf("findr v%s (go1.26)\n", version)
	default:
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`findr - Modern music collection manager, monitor, and downloader

Usage:
  findr <command> [arguments] [flags]

Commands:
  search <query>          Search for an artist on the configured indexer (Deezer/Qobuz)
  add <artist-name>       Find an artist, monitor them, and index all their albums & singles
  albums <artist-id>      List all indexed albums and releases for an artist
  artists                 List all monitored artists in your local database
  download <album-id>     Execute pipeline: download -> verify -> import -> navidrome scan
  scan                    Trigger a library scan on your Navidrome server
  version                 Print findr version

Flags:
  --config <path>         Path to findr YAML configuration (default: config.yaml)`)
}

func handleSearch(ctx context.Context, idx indexer.Indexer) {
	if len(os.Args) < 3 {
		fmt.Println("Usage: findr search <query>")
		os.Exit(1)
	}
	query := os.Args[2]
	fmt.Printf("Searching for '%s' using indexer '%s'...\n\n", query, idx.Name())

	artists, err := idx.SearchArtist(ctx, query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Search error: %v\n", err)
		os.Exit(1)
	}

	if len(artists) == 0 {
		fmt.Println("No artists found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSOURCE")
	for _, a := range artists {
		fmt.Fprintf(w, "%s\t%s\t%s\n", a.ID, a.Name, a.Source)
	}
	w.Flush()
}

func handleAdd(ctx context.Context, orch *orchestrator.Orchestrator) {
	if len(os.Args) < 3 {
		fmt.Println("Usage: findr add <artist-name>")
		os.Exit(1)
	}
	query := os.Args[2]
	fmt.Printf("Searching and adding artist '%s'...\n", query)

	artist, albums, err := orch.SearchAndAddArtist(ctx, query)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error adding artist: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\nSuccessfully added artist: %s (ID: %s)\n", artist.Name, artist.ID)
	fmt.Printf("Indexed %d releases (Albums, EPs, Singles):\n\n", len(albums))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tYEAR\tTYPE\tTITLE")
	for _, alb := range albums {
		yearStr := "-"
		if alb.Year > 0 {
			yearStr = fmt.Sprintf("%d", alb.Year)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", alb.ID, yearStr, alb.ReleaseType, alb.Title)
	}
	w.Flush()
}

func handleListArtists(ctx context.Context, repo storage.Repository) {
	artists, err := repo.ListArtists(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing artists: %v\n", err)
		os.Exit(1)
	}

	if len(artists) == 0 {
		fmt.Println("No monitored artists in database. Use 'findr add <artist>' to add one.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSOURCE\tMONITORED")
	for _, a := range artists {
		monStr := "yes"
		if !a.Monitored {
			monStr = "no"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.ID, a.Name, a.Source, monStr)
	}
	w.Flush()
}

func handleListAlbums(ctx context.Context, repo storage.Repository, idx indexer.Indexer) {
	if len(os.Args) < 3 {
		fmt.Println("Usage: findr albums <artist-id>")
		os.Exit(1)
	}
	artistID := os.Args[2]

	albums, err := repo.ListAlbumsByArtist(ctx, artistID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error querying database: %v\n", err)
		os.Exit(1)
	}

	if len(albums) == 0 {
		// Try fetching directly from indexer
		fmt.Println("Artist not found locally, querying indexer...")
		releases, err := idx.GetArtistReleases(ctx, artistID, indexer.ReleaseFilter{Limit: 100})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Indexer error: %v\n", err)
			os.Exit(1)
		}
		albums = releases
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tYEAR\tTYPE\tSTATUS\tTITLE")
	for _, alb := range albums {
		yearStr := "-"
		if alb.Year > 0 {
			yearStr = fmt.Sprintf("%d", alb.Year)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", alb.ID, yearStr, alb.ReleaseType, alb.Status, alb.Title)
	}
	w.Flush()
}

func handleDownload(ctx context.Context, orch *orchestrator.Orchestrator) {
	if len(os.Args) < 3 {
		fmt.Println("Usage: findr download <album-id>")
		os.Exit(1)
	}
	albumID := os.Args[2]
	fmt.Printf("Starting download pipeline for album %s...\n", albumID)

	progressFn := func(pct float64, msg string) {
		fmt.Printf("[%5.1f%%] %s\n", pct, msg)
	}

	match, imp, err := orch.ProcessAlbumPipeline(ctx, albumID, progressFn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nPipeline failed: %v\n", err)
		if match != nil {
			fmt.Printf("Verification Details: Confidence=%.2f, Passed=%v, Reason=%s\n",
				match.ConfidenceScore, match.Passed, match.Reason)
		}
		os.Exit(1)
	}

	fmt.Printf("\nSUCCESS: Album verified and imported into:\n%s\n", imp.AlbumDir)
}

func handleScan(ctx context.Context, navi *navidrome.Client) {
	if navi == nil {
		fmt.Println("Navidrome is not enabled in configuration.")
		os.Exit(1)
	}

	fmt.Println("Triggering Navidrome library scan...")
	status, err := navi.StartScan(ctx, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Scan failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Navidrome scan triggered. Current track count: %d, Scanning: %v\n", status.Count, status.Scanning)
}
