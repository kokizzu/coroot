package cache

import (
	"bytes"
	"fmt"
	"github.com/coroot/coroot-focus/model"
	"github.com/coroot/coroot-focus/timeseries"
	"k8s.io/klog"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type CompactionConfig struct {
	Interval   time.Duration `yaml:"interval"`
	WorkersNum int           `yaml:"workers_num"`

	Compactors []Compactor `yaml:"compactors"`
}

type Compactor struct {
	SrcChunkDuration timeseries.Duration `yaml:"src_chunk_duration_seconds"`
	DstChunkDuration timeseries.Duration `yaml:"dst_chunk_duration_seconds"`
}

var DefaultCompactionConfig = CompactionConfig{
	Interval:   time.Minute * 10,
	WorkersNum: 1,
	Compactors: []Compactor{
		{SrcChunkDuration: 3600, DstChunkDuration: 4 * 3600},
		{SrcChunkDuration: 4 * 3600, DstChunkDuration: 12 * 3600},
	},
}

type CompactionTask struct {
	queryHash string
	dstChunk  timeseries.Time
	src       []*ChunkInfo
	compactor Compactor
}

func (ct CompactionTask) String() string {
	src := make([]string, 0, len(ct.src))
	for _, s := range ct.src {
		src = append(src, strconv.Itoa(int(s.ts)))
	}
	return fmt.Sprintf(
		"compaction task %s [%s]:%d -> %d:%d",
		ct.queryHash, strings.Join(src, ","), ct.compactor.SrcChunkDuration, ct.dstChunk, ct.compactor.DstChunkDuration,
	)
}

func calcCompactionTasks(compactor Compactor, queryHash string, chunks map[string]*ChunkInfo) []*CompactionTask {
	tasks := map[timeseries.Time]*CompactionTask{}
	jitter := timeseries.Duration(queryJitter(queryHash).Seconds())
	for _, chunk := range chunks {
		if chunk.duration != compactor.SrcChunkDuration {
			continue
		}
		if chunk.lastTs != chunk.ts.Add(chunk.duration-chunk.step) { //incomplete
			continue
		}
		dstChunkTs := chunk.ts.Add(-jitter).Truncate(compactor.DstChunkDuration).Add(jitter)
		task := tasks[dstChunkTs]
		if task == nil {
			task = &CompactionTask{
				queryHash: queryHash,
				dstChunk:  dstChunkTs,
				compactor: compactor,
			}
			tasks[dstChunkTs] = task
		}
		task.src = append(task.src, chunk)
	}
	res := make([]*CompactionTask, 0, len(tasks))
	for _, task := range tasks {
		if len(task.src) == int(compactor.DstChunkDuration/compactor.SrcChunkDuration) {
			res = append(res, task)
		}
	}
	return res
}

func (c *Cache) compaction(cfg CompactionConfig) {
	if len(cfg.Compactors) == 0 {
		klog.Warningln("no compactors configured, deactivating compaction")
	}
	tasksCh := make(chan CompactionTask)

	for i := 0; i < cfg.WorkersNum; i++ {
		go func(ch <-chan CompactionTask) {
			klog.Infoln("compaction worker started")
			buf := &bytes.Buffer{}
			for t := range ch {
				if err := c.compact(t, buf); err != nil {
					klog.Errorln(err)
					continue
				}
			}
		}(tasksCh)
	}

	for range time.Tick(cfg.Interval) {
		klog.Infoln("compaction iteration started")
		var tasks []*CompactionTask
		c.lock.RLock()

		for queryHash, qData := range c.data {
			for _, cfg := range cfg.Compactors {
				tasks = append(tasks, calcCompactionTasks(cfg, queryHash, qData.chunksOnDisk)...)
			}
		}
		c.lock.RUnlock()
		for i, t := range tasks {
			c.pendingCompactions.Set(float64(len(tasks) - i - 1))
			tasksCh <- *t
		}
	}
}

func (c *Cache) compact(t CompactionTask, buf *bytes.Buffer) error {
	if len(t.src) == 0 {
		return fmt.Errorf("no src chunks")
	}
	start := time.Now()
	metrics := map[uint64]model.MetricValues{}
	sort.Slice(t.src, func(i, j int) bool {
		return t.src[i].ts < t.src[j].ts
	})
	dstStep := t.src[0].step
	dst := NewChunk(
		t.dstChunk,
		t.dstChunk.Add(t.compactor.DstChunkDuration-dstStep),
		t.compactor.DstChunkDuration,
		dstStep,
		buf,
	)
	for _, i := range t.src {
		src, err := NewChunkFromInfo(i)
		if err != nil {
			return fmt.Errorf("failed to open chunk for compaction: %s", err)
		}
		err = src.ReadMetrics(dst.from, dst.to, dst.step, metrics)
		src.Close()
		if err != nil {
			return fmt.Errorf("failed to read metrics from src chunk for compaction: %s", err)
		}
	}

	for _, m := range metrics {
		if err := dst.WriteMetric(m); err != nil {
			return err
		}
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	if err := c.saveChunk(t.queryHash, dst); err != nil {
		return err
	}
	qData := c.data[t.queryHash]
	if qData == nil {
		klog.Errorf("query data not found: %s", t.queryHash)
	} else {
		for _, src := range t.src {
			klog.Infoln("deleting chunk after compaction:", src.path)
			if err := os.Remove(src.path); err != nil {
				klog.Errorf("failed to delete chunk %s: %s", src.path, err)
			}
			delete(qData.chunksOnDisk, src.path)
		}
	}
	c.compactedChunks.WithLabelValues(
		strconv.Itoa(int(t.compactor.SrcChunkDuration)),
		strconv.Itoa(int(t.compactor.DstChunkDuration)),
	).Inc()
	klog.Infoln(t.String(), "done in", time.Since(start))
	return nil
}
