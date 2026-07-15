package proxy

// Durable request-log persistence (mirrors persist.go's AuditStore pattern but
// for RequestLog, so the admin /logs view survives restarts).
//
// Writes are asynchronous and non-blocking: appendRequestLog hands each entry to
// recordLog, which enqueues onto a buffered channel (bufSize 4096). When the
// channel is full the entry is dropped and counted (never blocks the request
// path). A single writer goroutine drains the channel to a JSON-lines file,
// flushing after each entry so a crash loses at most the in-flight record.
//
// SECURITY: RequestLog carries ApiKeyID/ApiKeyName only (never the cleartext
// key) — the persisted jsonl is safe to inspect and ship off-host.
//
// KIRO_LOG_PERSIST_DISABLE turns persistence off entirely (the in-memory ring
// still works; it just isn't seeded from disk or written to disk).
import (
	"bufio"
	"encoding/json"
	"kiro-go/logger"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// logBackend is the persistence interface for request logs (mirrors auditBackend).
type logBackend interface {
	write(RequestLog) error
	queryRecent(limit int) ([]RequestLog, error)
	Close() error
}

// ---- JSON-lines backend ----

// jsonlLogMaxTailBytes bounds the tail read in queryRecent (4 MiB — far more
// than limit*entrySize for any sane limit, keeps the read cheap on a large file).
const jsonlLogMaxTailBytes int64 = 4 << 20

type jsonlLog struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

func newJSONLLog(path string) (*jsonlLog, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &jsonlLog{f: f, w: bufio.NewWriter(f)}, nil
}

func (j *jsonlLog) write(e RequestLog) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := j.w.Write(b); err != nil {
		return err
	}
	if err := j.w.WriteByte('\n'); err != nil {
		return err
	}
	return j.w.Flush() // flush each entry so a crash loses at most the in-flight write
}

// queryRecent returns the most recent up-to-limit entries (file order = oldest
// first, since the file is append-only) by reading the file tail with a sliding
// window — identical strategy to jsonlAudit.queryRecent.
func (j *jsonlLog) queryRecent(limit int) ([]RequestLog, error) {
	if limit <= 0 {
		limit = 100
	}
	j.mu.Lock()
	path := j.f.Name()
	j.mu.Unlock()

	rf, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer rf.Close()

	st, err := rf.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	off := int64(0)
	if size > jsonlLogMaxTailBytes {
		off = size - jsonlLogMaxTailBytes
	}
	if _, err := rf.Seek(off, 0); err != nil {
		return nil, err
	}

	var ring []RequestLog
	sc := bufio.NewScanner(rf)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e RequestLog
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // skip malformed/partial line (e.g. mid-write tail)
		}
		ring = append(ring, e)
		if len(ring) > limit {
			ring = ring[len(ring)-limit:]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return ring, nil
}

func (j *jsonlLog) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.w.Flush(); err != nil {
		_ = j.f.Close()
		return err
	}
	return j.f.Close()
}

// ---- Async store ----

// LogStore drains RequestLog entries from a buffered channel to its backend on
// a single writer goroutine. Record never blocks the caller.
type LogStore struct {
	ch        chan RequestLog
	backend   logBackend
	wg        sync.WaitGroup
	closeOnce sync.Once
	dropped   int64 // atomic; incremented when the channel is full
}

func NewLogStore(backend logBackend, bufSize int) *LogStore {
	if bufSize <= 0 {
		bufSize = 4096
	}
	s := &LogStore{ch: make(chan RequestLog, bufSize), backend: backend}
	s.wg.Add(1)
	go s.loop()
	return s
}

func (s *LogStore) loop() {
	defer s.wg.Done()
	for e := range s.ch {
		if err := s.backend.write(e); err != nil {
			logger.Warnf("[LogPersist] write failed: %v", err)
		}
	}
}

// Record enqueues an entry without blocking; a full buffer drops the entry and
// bumps the dropped counter.
func (s *LogStore) Record(e RequestLog) {
	select {
	case s.ch <- e:
	default:
		n := atomic.AddInt64(&s.dropped, 1)
		// Log the first drop and every 1000th thereafter so operators notice
		// persistent backpressure without flooding the log.
		if n == 1 || n%1000 == 0 {
			logger.Warnf("[LogPersist] channel full — dropped %d log entries total", n)
		}
	}
}

// Close stops the writer, flushing buffered entries, and closes the backend.
func (s *LogStore) Close() {
	s.closeOnce.Do(func() {
		close(s.ch)
		s.wg.Wait()
		if err := s.backend.Close(); err != nil {
			logger.Warnf("[LogPersist] close failed: %v", err)
		}
	})
}

// ---- Package-level wiring ----

var globalLogStore *LogStore

// InitLogStore initializes the process-wide log store under dir. It is a no-op
// when KIRO_LOG_PERSIST_DISABLE is set.
func InitLogStore(dir string) {
	if strings.TrimSpace(os.Getenv("KIRO_LOG_PERSIST_DISABLE")) != "" {
		logger.Infof("[LogPersist] disabled via KIRO_LOG_PERSIST_DISABLE")
		return
	}
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Warnf("[LogPersist] cannot create dir %s: %v", dir, err)
		return
	}
	jl, err := newJSONLLog(filepath.Join(dir, "kiro-logs.jsonl"))
	if err != nil {
		logger.Errorf("[LogPersist] disabled: cannot open JSONL backend: %v", err)
		return
	}
	globalLogStore = NewLogStore(jl, 4096)
	logger.Infof("[LogPersist] enabled (dir=%s)", dir)
}

// CloseLogStore flushes and closes the log store, if any.
func CloseLogStore() {
	if globalLogStore != nil {
		globalLogStore.Close()
	}
}

// recordLog enqueues a request log for async persistence. Nil-safe + non-blocking.
func recordLog(e RequestLog) {
	if globalLogStore != nil {
		globalLogStore.Record(e)
	}
}

// LoadRecentLogs returns the most recent up-to-limit persisted entries (oldest
// first), or nil if persistence is disabled or the store is empty.
func LoadRecentLogs(limit int) []RequestLog {
	if globalLogStore == nil {
		return nil
	}
	logs, err := globalLogStore.backend.queryRecent(limit)
	if err != nil {
		logger.Warnf("[LogPersist] LoadRecentLogs failed: %v", err)
		return nil
	}
	return logs
}
