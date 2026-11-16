package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/findr-app/findr/internal/domain"
)

type queueItem struct {
	AlbumID string
	JobID   string
}

// QueueManager manages concurrent album downloads with configurable limits.
type QueueManager struct {
	mu            sync.Mutex
	cond          *sync.Cond
	orch          *Orchestrator
	maxConcurrent int
	runningCount  int
	queue         []queueItem
	queuedSet     map[string]string             // albumID -> jobID
	cancels       map[string]context.CancelFunc // albumID -> cancel func
	stopped       bool
}

// NewQueueManager initializes QueueManager with specified concurrency limit.
func NewQueueManager(orch *Orchestrator, maxConcurrent int) *QueueManager {
	if maxConcurrent <= 0 {
		maxConcurrent = 2
	}
	qm := &QueueManager{
		orch:          orch,
		maxConcurrent: maxConcurrent,
		queue:         make([]queueItem, 0),
		queuedSet:     make(map[string]string),
		cancels:       make(map[string]context.CancelFunc),
	}
	qm.cond = sync.NewCond(&qm.mu)
	return qm
}

// Start launches the background dispatch loop.
func (qm *QueueManager) Start() {
	go qm.dispatchLoop()
}

// SetMaxConcurrent updates the concurrency limit dynamically and notifies waiting workers.
func (qm *QueueManager) SetMaxConcurrent(n int) {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if n <= 0 {
		n = 2
	}
	if n != qm.maxConcurrent {
		fmt.Printf("[findr Queue] Concurrency limit updated: %d -> %d\n", qm.maxConcurrent, n)
		qm.maxConcurrent = n
		qm.cond.Broadcast()
	}
}

// GetConcurrency returns the currently configured concurrency limit, active count, and pending count.
func (qm *QueueManager) GetConcurrency() (max int, active int, pending int) {
	qm.mu.Lock()
	defer qm.mu.Unlock()
	return qm.maxConcurrent, qm.runningCount, len(qm.queue)
}

// Enqueue adds an album to the download queue.
func (qm *QueueManager) Enqueue(albumID string) (string, error) {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	if qm.stopped {
		return "", fmt.Errorf("queue is stopped")
	}

	// Check if already in queue or currently running
	if jobID, exists := qm.queuedSet[albumID]; exists {
		return jobID, nil
	}

	ctx := context.Background()
	album, err := qm.orch.repo.GetAlbum(ctx, albumID)
	if err != nil {
		return "", fmt.Errorf("retrieve album %s: %w", albumID, err)
	}
	if album == nil {
		return "", fmt.Errorf("album %s not found", albumID)
	}

	// Check if album is already being downloaded
	if album.Status == domain.StatusDownloading || album.Status == domain.StatusVerifying || album.Status == domain.StatusImporting {
		return "", nil
	}

	qm.orch.mu.RLock()
	cfg := qm.orch.cfg
	qm.orch.mu.RUnlock()

	jobID := fmt.Sprintf("dl-%s-%d", album.ID, time.Now().Unix())
	stagingDir := filepath.Join(cfg.Library.DownloadPath, fmt.Sprintf("%s-%d", album.ID, time.Now().Unix()))

	downloadJob := &domain.DownloadJob{
		ID:            jobID,
		AlbumID:       album.ID,
		Downloader:    cfg.Downloader.Preferred,
		DownloadDir:   stagingDir,
		TargetQuality: domain.AudioQuality(cfg.Downloader.Qobuz.TargetQuality),
		Status:        domain.StatusPending,
		Progress:      0.0,
		CreatedAt:     time.Now(),
	}
	_ = qm.orch.repo.SaveDownloadJob(ctx, downloadJob)

	album.Status = domain.StatusPending
	_ = qm.orch.repo.SaveAlbum(ctx, album)

	item := queueItem{AlbumID: albumID, JobID: jobID}
	qm.queue = append(qm.queue, item)
	qm.queuedSet[albumID] = jobID

	fmt.Printf("[findr Queue] Enqueued '%s - %s' (Job: %s, Queue depth: %d, Running: %d/%d)\n",
		album.ArtistName, album.Title, jobID, len(qm.queue), qm.runningCount, qm.maxConcurrent)

	qm.cond.Signal()
	return jobID, nil
}

// EnqueueBatch enqueues multiple albums at once.
func (qm *QueueManager) EnqueueBatch(albumIDs []string) (int, error) {
	count := 0
	for _, id := range albumIDs {
		if _, err := qm.Enqueue(id); err == nil {
			count++
		}
	}
	return count, nil
}

// Cancel cancels an in-flight or pending download job.
func (qm *QueueManager) Cancel(albumID string) error {
	qm.mu.Lock()
	defer qm.mu.Unlock()

	// If running, trigger context cancel
	if cancel, ok := qm.cancels[albumID]; ok {
		cancel()
		delete(qm.cancels, albumID)
	}

	// If pending in queue, remove from queue
	for i, it := range qm.queue {
		if it.AlbumID == albumID {
			qm.queue = append(qm.queue[:i], qm.queue[i+1:]...)
			break
		}
	}

	jobID := qm.queuedSet[albumID]
	delete(qm.queuedSet, albumID)

	// Update job & album status in DB
	ctx := context.Background()
	if jobID != "" {
		if job, err := qm.orch.repo.GetDownloadJob(ctx, jobID); err == nil && job != nil {
			job.Status = domain.StatusFailed
			job.ErrorMessage = "Download cancelled by user"
			now := time.Now()
			job.CompletedAt = &now
			_ = qm.orch.repo.SaveDownloadJob(ctx, job)
		}
	}
	if album, err := qm.orch.repo.GetAlbum(ctx, albumID); err == nil && album != nil {
		album.Status = domain.StatusFailed
		_ = qm.orch.repo.SaveAlbum(ctx, album)
	}

	qm.cond.Signal()
	return nil
}

// dispatchLoop runs in the background and launches workers up to maxConcurrent.
func (qm *QueueManager) dispatchLoop() {
	for {
		qm.mu.Lock()
		for len(qm.queue) == 0 || qm.runningCount >= qm.maxConcurrent {
			if qm.stopped {
				qm.mu.Unlock()
				return
			}
			qm.cond.Wait()
			if qm.stopped {
				qm.mu.Unlock()
				return
			}
		}

		item := qm.queue[0]
		qm.queue = qm.queue[1:]
		qm.runningCount++

		ctx, cancel := context.WithCancel(context.Background())
		qm.cancels[item.AlbumID] = cancel
		qm.mu.Unlock()

		go func(it queueItem, jobCtx context.Context) {
			defer func() {
				qm.mu.Lock()
				qm.runningCount--
				delete(qm.queuedSet, it.AlbumID)
				delete(qm.cancels, it.AlbumID)
				qm.cond.Signal()
				qm.mu.Unlock()
			}()

			_, _, err := qm.orch.ProcessAlbumPipelineWithJob(jobCtx, it.AlbumID, it.JobID, nil)
			if err != nil {
				fmt.Printf("[findr Queue ERROR] Album pipeline failed for %s: %v\n", it.AlbumID, err)
			}
		}(item, ctx)
	}
}

// Stop shuts down the queue and cancels in-flight jobs.
func (qm *QueueManager) Stop() {
	qm.mu.Lock()
	qm.stopped = true
	for _, cancel := range qm.cancels {
		cancel()
	}
	qm.cancels = make(map[string]context.CancelFunc)
	qm.cond.Broadcast()
	qm.mu.Unlock()
}
