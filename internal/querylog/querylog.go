// Package querylog persists answered queries as JSON Lines files and reads
// them back newest first for the control page.
//
// Writing never blocks the caller: Append hands the entry to a background
// goroutine through a bounded queue and drops it when the queue is full. The
// goroutine formats entries, appends them to the current segment file
// ("queries-<UTC time>.jsonl"), rotates to a new segment at SegmentBytes or
// once the segment is older than the rotation period, and removes the oldest
// segments once the directory exceeds MaxBytes or a segment is older than
// MaxAge. Rotating by time keeps MaxAge honest at low query rates, where a
// segment would otherwise take weeks to fill and the current segment is never
// removed.
//
// Reading walks the segments from newest to oldest, each one backwards in
// fixed-size blocks, and returns a cursor ("<segment>@<offset>") at which the
// next page continues. A single read examines a bounded number of bytes, so a
// filter that matches nothing cannot make one request scan the whole
// directory; the caller receives a cursor and decides whether to continue.
package querylog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Record is one answered query as stored on disk and returned by the API.
// Queries refused locally before routing carry the rule "_invalid".
type Record struct {
	Seq        uint64    `json:"seq,omitempty"` // in-memory ring only; not stable across restarts
	Time       time.Time `json:"time"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`            // empty when the query carried no readable question
	Class      string    `json:"class,omitempty"` // set only for classes other than IN
	Proto      string    `json:"proto"`
	Rule       string    `json:"rule"`
	Set        string    `json:"set,omitempty"`
	Match      string    `json:"match,omitempty"`
	Target     string    `json:"target"`
	RCode      string    `json:"rcode"`
	Truncated  bool      `json:"truncated,omitempty"`
	DurationMS float64   `json:"duration_ms"`
}

// Observer receives write-side events, typically to update metrics. Methods
// are called from the writer goroutine, except QueryLogDropped, which is
// called from Append.
type Observer interface {
	QueryLogWritten()
	QueryLogDropped()
	QueryLogWriteError()
}

// Options configures a Log. Zero values select the defaults.
type Options struct {
	MaxBytes      int64         // total size of all segments; required
	MaxAge        time.Duration // segments last written before now-MaxAge are removed; required
	SegmentBytes  int64         // rotate when the current segment would exceed this (default 16 MiB)
	Queue         int           // entries buffered between Append and the writer (default 65536)
	FlushInterval time.Duration // how often buffered lines reach the file (default 1s)
	ScanBytes     int64         // bytes one Read may examine (default 32 MiB)
	Observer      Observer      // may be nil
}

const (
	defaultSegmentBytes = 16 << 20
	defaultQueue        = 65536
	defaultFlush        = time.Second
	defaultScanBytes    = 32 << 20
	pruneInterval       = time.Hour
	maxRotatePeriod     = 24 * time.Hour
	retryAfterFailure   = 5 * time.Second
	readBlock           = 64 << 10
	maxLine             = 64 << 10 // longer "lines" are corruption and are skipped
	timeLayout          = "20060102T150405.000000000Z"
	filePrefix          = "queries-"
	fileSuffix          = ".jsonl"
)

// segmentRE matches segment file names; cursors are checked against it so a
// cursor can only name a file inside the log directory.
var segmentRE = regexp.MustCompile(`^queries-\d{8}T\d{6}\.\d{9}Z\.jsonl$`)

// ErrBadCursor is returned by Read for a cursor it did not produce.
var ErrBadCursor = errors.New("invalid cursor")

// Log is an open query log. E is the caller's entry type; format converts it
// to a Record on the writer goroutine, so the caller's hot path performs no
// formatting.
type Log[E any] struct {
	dir    string
	opts   Options
	format func(*E) Record
	ch     chan E
	stop   chan struct{}
	done   chan struct{}
	closed atomic.Bool
	once   sync.Once

	mu      sync.Mutex // guards the fields below
	f       *os.File
	bw      *bufio.Writer
	cur     string    // current segment name, "" when none is open
	curSize int64     // bytes written to cur, including buffered bytes
	curAt   time.Time // creation time of cur, from its name
	retryAt time.Time // after a failure, no new segment is opened before this
	oldest  struct {
		name string
		at   time.Time
	}

	dropped atomic.Uint64
	errors  atomic.Uint64
	lastErr atomic.Pointer[string]
}

// Open creates dir if needed, checks that it is writable by opening the
// segment to append to, and starts the writer goroutine.
func Open[E any](dir string, format func(*E) Record, opts Options) (*Log[E], error) {
	if opts.MaxBytes <= 0 || opts.MaxAge <= 0 {
		return nil, errors.New("querylog: MaxBytes and MaxAge are required")
	}
	if opts.SegmentBytes <= 0 {
		opts.SegmentBytes = defaultSegmentBytes
	}
	opts.SegmentBytes = min(opts.SegmentBytes, opts.MaxBytes)
	if opts.Queue <= 0 {
		opts.Queue = defaultQueue
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = defaultFlush
	}
	if opts.ScanBytes <= 0 {
		opts.ScanBytes = defaultScanBytes
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	l := &Log[E]{dir: dir, opts: opts, format: format, ch: make(chan E, opts.Queue),
		stop: make(chan struct{}), done: make(chan struct{})}
	if err := l.openLatest(); err != nil {
		return nil, err
	}
	l.prune(time.Now())
	go l.run()
	return l, nil
}

// Dir returns the directory the log writes to.
func (l *Log[E]) Dir() string { return l.dir }

// Append queues e for writing. It never blocks: when the queue is full, or
// after Close, the entry is dropped and counted.
func (l *Log[E]) Append(e E) {
	if l.closed.Load() {
		return
	}
	select {
	case l.ch <- e:
	default:
		l.dropped.Add(1)
		if l.opts.Observer != nil {
			l.opts.Observer.QueryLogDropped()
		}
	}
}

// Close writes every queued entry, flushes, and closes the current segment.
// It is safe to call more than once.
func (l *Log[E]) Close() error {
	l.once.Do(func() {
		l.closed.Store(true)
		close(l.stop)
	})
	<-l.done
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closeFile()
}

func (l *Log[E]) run() {
	defer close(l.done)
	flush := time.NewTicker(l.opts.FlushInterval)
	defer flush.Stop()
	lastPrune := time.Now()
	for {
		select {
		case e := <-l.ch:
			l.mu.Lock()
			l.write(&e)
			// Write whatever else is already queued under the same lock.
			for n := len(l.ch); n > 0; n-- {
				e := <-l.ch
				l.write(&e)
			}
			l.mu.Unlock()
		case now := <-flush.C:
			l.mu.Lock()
			l.flushLocked()
			l.mu.Unlock()
			if now.Sub(lastPrune) >= pruneInterval {
				lastPrune = now
				l.prune(now)
			}
		case <-l.stop:
			l.mu.Lock()
			for {
				select {
				case e := <-l.ch:
					l.write(&e)
					continue
				default:
				}
				break
			}
			l.flushLocked()
			l.mu.Unlock()
			return
		}
	}
}

// write appends one entry. Called with mu held.
func (l *Log[E]) write(e *E) {
	rec := l.format(e)
	rec.Seq = 0
	line, err := json.Marshal(&rec)
	if err != nil {
		l.fail(err)
		return
	}
	line = append(line, '\n')
	if l.f == nil && time.Now().Before(l.retryAt) {
		// The last write failed (a full disk, say). Opening a new segment for
		// every record would litter the directory with empty files; the record
		// is lost and counted instead, and a new segment is tried shortly.
		l.lose()
		return
	}
	if l.f == nil || l.curSize+int64(len(line)) > l.opts.SegmentBytes || time.Since(l.curAt) >= l.rotatePeriod() {
		rotated := l.f != nil
		if err := l.rotate(rec.Time); err != nil {
			l.fail(err)
			return
		}
		if rotated {
			go l.prune(time.Now())
		}
	}
	if _, err := l.bw.Write(line); err != nil {
		l.fail(err)
		// Start a new segment on the next write instead of appending after a
		// partial line.
		_ = l.closeFile()
		return
	}
	l.curSize += int64(len(line))
	if l.opts.Observer != nil {
		l.opts.Observer.QueryLogWritten()
	}
}

// fail records a write failure; the caller closes the segment, and the next
// one is opened after retryAfterFailure. Called with mu held.
func (l *Log[E]) fail(err error) {
	msg := err.Error()
	l.lastErr.Store(&msg)
	l.retryAt = time.Now().Add(retryAfterFailure)
	l.lose()
}

// lose counts a record lost to a failed write.
func (l *Log[E]) lose() {
	l.errors.Add(1)
	if l.opts.Observer != nil {
		l.opts.Observer.QueryLogWriteError()
	}
}

// rotatePeriod is the longest time one segment stays current: a day, or a
// quarter of MaxAge when that is shorter, so that age-based pruning removes
// records at most a quarter of MaxAge late.
func (l *Log[E]) rotatePeriod() time.Duration {
	return min(maxRotatePeriod, l.opts.MaxAge/4)
}

// flushLocked writes buffered lines to the file. Called with mu held.
func (l *Log[E]) flushLocked() {
	if l.bw == nil || l.bw.Buffered() == 0 {
		return
	}
	if err := l.bw.Flush(); err != nil {
		l.fail(err)
		_ = l.closeFile()
	}
}

// closeFile flushes and closes the current segment. Called with mu held.
func (l *Log[E]) closeFile() error {
	if l.f == nil {
		return nil
	}
	err := l.bw.Flush()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f, l.bw, l.cur, l.curSize = nil, nil, "", 0
	return err
}

// rotate closes the current segment and creates a new one named after at.
// Called with mu held.
func (l *Log[E]) rotate(at time.Time) error {
	_ = l.closeFile()
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()
	if names, err := l.segments(); err == nil && len(names) > 0 {
		// Names must sort in creation order even if the clock steps back.
		if last, ok := segmentTime(names[len(names)-1]); ok && !at.After(last) {
			at = last.Add(time.Nanosecond)
		}
	}
	for range 8 {
		name := filePrefix + at.Format(timeLayout) + fileSuffix
		f, err := os.OpenFile(filepath.Join(l.dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o640)
		if errors.Is(err, os.ErrExist) {
			at = at.Add(time.Nanosecond)
			continue
		}
		if err != nil {
			return err
		}
		l.f, l.bw, l.cur, l.curSize, l.curAt = f, bufio.NewWriterSize(f, 32<<10), name, 0, time.Now()
		return nil
	}
	return errors.New("querylog: could not create a unique segment name")
}

// openLatest opens the newest segment for appending when it has room and is
// younger than the rotation period, or creates a new one. It is the
// writability check performed by Open.
func (l *Log[E]) openLatest() error {
	names, err := l.segments()
	if err != nil {
		return err
	}
	if len(names) > 0 {
		name := names[len(names)-1]
		p := filepath.Join(l.dir, name)
		created, _ := segmentTime(name)
		if st, err := os.Stat(p); err == nil && st.Size() < l.opts.SegmentBytes && time.Since(created) < l.rotatePeriod() {
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				return err
			}
			l.f, l.bw, l.cur, l.curSize, l.curAt = f, bufio.NewWriterSize(f, 32<<10), name, st.Size(), created
			// A crash may have left a partial last line; terminate it so the next
			// record starts on its own line.
			if st.Size() > 0 && lastByte(p, st.Size()) != '\n' {
				_, _ = l.bw.WriteString("\n")
				l.curSize++
			}
			return l.bw.Flush()
		}
	}
	return l.rotate(time.Now())
}

func lastByte(p string, size int64) byte {
	f, err := os.Open(p)
	if err != nil {
		return 0
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], size-1); err != nil {
		return 0
	}
	return b[0]
}

// segments lists segment names, oldest first.
func (l *Log[E]) segments() ([]string, error) {
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.Type().IsRegular() && segmentRE.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

func segmentTime(name string) (time.Time, bool) {
	s := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), fileSuffix)
	t, err := time.Parse(timeLayout, s)
	return t, err == nil
}

// prune removes segments last written before now-MaxAge, then the oldest
// segments until the total fits MaxBytes. The current segment is never
// removed.
func (l *Log[E]) prune(now time.Time) {
	names, err := l.segments()
	if err != nil {
		return
	}
	l.mu.Lock()
	cur, curSize := l.cur, l.curSize
	l.mu.Unlock()
	type seg struct {
		name  string
		size  int64
		mtime time.Time
	}
	segs := make([]seg, 0, len(names))
	var total int64
	for _, n := range names {
		st, err := os.Stat(filepath.Join(l.dir, n))
		if err != nil {
			continue
		}
		s := seg{n, st.Size(), st.ModTime()}
		if n == cur {
			s.size, s.mtime = curSize, now
		}
		segs = append(segs, s)
		total += s.size
	}
	cutoff := now.Add(-l.opts.MaxAge)
	for _, s := range segs {
		if s.name == cur || (total <= l.opts.MaxBytes && !s.mtime.Before(cutoff)) {
			continue
		}
		if err := os.Remove(filepath.Join(l.dir, s.name)); err == nil || errors.Is(err, os.ErrNotExist) {
			total -= s.size
		}
	}
}

// Stats describes the log for the status page.
type Stats struct {
	Dir       string
	Files     int
	Bytes     int64
	Oldest    time.Time // time of the oldest stored record; zero when empty
	Dropped   uint64
	Errors    uint64
	LastError string
}

// Stats reports the files on disk and the write-side counters.
func (l *Log[E]) Stats() Stats {
	st := Stats{Dir: l.dir, Dropped: l.dropped.Load(), Errors: l.errors.Load()}
	if p := l.lastErr.Load(); p != nil {
		st.LastError = *p
	}
	// Flush so that the sizes and the oldest record describe the same bytes a
	// Read would see.
	l.mu.Lock()
	l.flushLocked()
	cur, curSize := l.cur, l.curSize
	l.mu.Unlock()
	names, err := l.segments()
	if err != nil {
		return st
	}
	for _, n := range names {
		if n == cur {
			st.Bytes += curSize
			st.Files++
			continue
		}
		if fi, err := os.Stat(filepath.Join(l.dir, n)); err == nil {
			st.Bytes += fi.Size()
			st.Files++
		}
	}
	for _, n := range names {
		if t, ok := l.firstTime(n); ok {
			st.Oldest = t
			break
		}
	}
	return st
}

// firstTime returns the time of the first record in a segment, caching the
// answer for the oldest segment, which Stats asks about every few seconds.
func (l *Log[E]) firstTime(name string) (time.Time, bool) {
	l.mu.Lock()
	if l.oldest.name == name {
		at := l.oldest.at
		l.mu.Unlock()
		return at, true
	}
	l.mu.Unlock()
	f, err := os.Open(filepath.Join(l.dir, name))
	if err != nil {
		return time.Time{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxLine))
	sc.Buffer(make([]byte, 0, 4096), maxLine)
	for sc.Scan() {
		var r struct {
			Time time.Time `json:"time"`
		}
		if json.Unmarshal(sc.Bytes(), &r) == nil && !r.Time.IsZero() {
			l.mu.Lock()
			l.oldest.name, l.oldest.at = name, r.Time
			l.mu.Unlock()
			return r.Time, true
		}
	}
	return time.Time{}, false
}

// Page is one Read result. Next is empty when no older records exist.
type Page struct {
	Records []Record
	Next    string
}

// Filter selects records. Contains lists byte strings that must all occur in
// a record's raw JSON line; it is checked before decoding, so a selective
// filter scans cheaply, and it must be exact under JSON encoding (plain ASCII
// without quotes or backslashes). Keep, when set, decides on the decoded
// record.
type Filter struct {
	Contains [][]byte
	Keep     func(*Record) bool
}

func (f *Filter) match(line []byte) (Record, bool) {
	for _, c := range f.Contains {
		if !bytes.Contains(line, c) {
			return Record{}, false
		}
	}
	var rec Record
	if json.Unmarshal(line, &rec) != nil {
		return Record{}, false
	}
	return rec, f.Keep == nil || f.Keep(&rec)
}

// Read returns up to limit records, newest first, that f selects. An empty
// cursor starts at the newest record; otherwise cursor is a Next value from an
// earlier Page. Fewer than limit records with a
// non-empty Next means the scan budget ran out before the filter found enough.
//
// Records written before the call are visible: the writer's buffer is flushed
// first. Records appended while Read runs are not, which keeps cursors stable.
func (l *Log[E]) Read(cursor string, limit int, f Filter) (Page, error) {
	startName, startOff, err := parseCursor(cursor)
	if err != nil {
		return Page{}, err
	}
	l.mu.Lock()
	l.flushLocked()
	cur, curSize := l.cur, l.curSize
	l.mu.Unlock()
	names, err := l.segments()
	if err != nil {
		return Page{}, err
	}

	// Pick the segment to start in. A cursor naming a segment that has since
	// been pruned continues in the newest segment older than it.
	i := len(names) - 1
	if startName != "" {
		i, _ = slices.BinarySearch(names, startName)
		if i == len(names) || names[i] != startName {
			i--
			startOff = -1
		}
	}

	page := Page{Records: []Record{}}
	var scanned int64
	for ; i >= 0; i-- {
		name := names[i]
		size := int64(-1)
		if name == cur {
			size = curSize
		}
		end := startOff
		startOff = -1
		next, done, err := l.scanSegment(name, size, end, limit, &f, &page, &scanned)
		if err != nil {
			continue // pruned while reading, or unreadable: move on
		}
		if done {
			if next > 0 || i > 0 {
				page.Next = name + "@" + strconv.FormatInt(next, 10)
			}
			return page, nil
		}
	}
	return page, nil
}

// scanSegment reads one segment backwards from end (-1 means the end of the
// file, capped at size when size >= 0). It returns done when the page is full
// or the scan budget is spent, with next the offset at which to resume.
func (l *Log[E]) scanSegment(name string, size, end int64, limit int, f *Filter,
	page *Page, scanned *int64) (next int64, done bool, err error) {
	file, err := os.Open(filepath.Join(l.dir, name))
	if err != nil {
		return 0, false, err
	}
	defer file.Close()
	fi, err := file.Stat()
	if err != nil {
		return 0, false, err
	}
	limitEnd := fi.Size()
	if size >= 0 {
		limitEnd = min(limitEnd, size)
	}
	if end < 0 || end > limitEnd {
		end = limitEnd
	}

	pos := end
	var tail []byte // start of a line whose beginning lies before pos
	buf := make([]byte, readBlock)
	for pos > 0 || len(tail) > 0 {
		var data []byte
		if pos > 0 {
			n := min(int64(readBlock), pos)
			pos -= n
			if _, err := file.ReadAt(buf[:n], pos); err != nil && !errors.Is(err, io.EOF) {
				return 0, false, err
			}
			data = append(append(make([]byte, 0, int(n)+len(tail)), buf[:n]...), tail...)
		} else {
			data, tail = tail, nil
		}
		tail = nil
		stop := len(data)
		for {
			j := bytes.LastIndexByte(data[:stop], '\n')
			if j < 0 && pos > 0 {
				if stop <= maxLine {
					tail = append([]byte(nil), data[:stop]...)
				}
				break
			}
			line := data[j+1 : stop]
			lineStart := pos + int64(j+1)
			*scanned += int64(len(line)) + 1
			if len(line) > 0 {
				if rec, ok := f.match(line); ok {
					page.Records = append(page.Records, rec)
				}
			}
			if len(page.Records) >= limit || *scanned >= l.opts.ScanBytes {
				return lineStart, true, nil
			}
			if j < 0 {
				break
			}
			stop = j
		}
	}
	return 0, false, nil
}

func parseCursor(c string) (name string, off int64, err error) {
	if c == "" {
		return "", -1, nil
	}
	name, offStr, ok := strings.Cut(c, "@")
	if !ok || !segmentRE.MatchString(name) {
		return "", 0, ErrBadCursor
	}
	off, err = strconv.ParseInt(offStr, 10, 64)
	if err != nil || off < 0 {
		return "", 0, ErrBadCursor
	}
	return name, off, nil
}

// String is used in logs.
func (s Stats) String() string {
	return fmt.Sprintf("%s: %d files, %d bytes", s.Dir, s.Files, s.Bytes)
}
