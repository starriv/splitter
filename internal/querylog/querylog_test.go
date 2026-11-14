package querylog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type entry struct {
	n    int
	rule string
	at   time.Time
}

var base = time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)

func format(e *entry) Record {
	return Record{Time: e.at, Name: fmt.Sprintf("q%d.example", e.n), Type: "A", Proto: "udp",
		Rule: e.rule, Target: "up", RCode: "NOERROR", Seq: 99}
}

var observers sync.Map // *Log[entry] -> *countObs

func open(t *testing.T, dir string, opts Options) *Log[entry] {
	t.Helper()
	obs := &countObs{}
	opts.Observer = obs
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 1 << 30
	}
	if opts.MaxAge == 0 {
		opts.MaxAge = 24 * time.Hour
	}
	if opts.Queue == 0 {
		opts.Queue = 100000
	}
	l, err := Open(dir, format, opts)
	if err != nil {
		t.Fatal(err)
	}
	observers.Store(l, obs)
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// appendN appends n entries and waits until the writer has taken all of them.
func appendN(t *testing.T, l *Log[entry], from, n int, rule string) {
	t.Helper()
	v, _ := observers.Load(l)
	obs := v.(*countObs)
	want := obs.written.Load() + obs.errs.Load() + int64(n)
	for i := from; i < from+n; i++ {
		l.Append(entry{n: i, rule: rule, at: base.Add(time.Duration(i) * time.Second)})
	}
	deadline := time.Now().Add(5 * time.Second)
	for obs.written.Load()+obs.errs.Load() < want {
		if time.Now().After(deadline) {
			t.Fatal("writer did not write every entry")
		}
		time.Sleep(time.Millisecond)
	}
}

// readAll pages through the whole log and returns the record numbers.
func readAll(t *testing.T, l *Log[entry], limit int, keep func(*Record) bool) []int {
	t.Helper()
	var out []int
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10000 {
			t.Fatal("paging does not terminate")
		}
		p, err := l.Read(cursor, limit, Filter{Keep: keep})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range p.Records {
			var n int
			fmt.Sscanf(r.Name, "q%d.example", &n)
			out = append(out, n)
		}
		if p.Next == "" {
			return out
		}
		cursor = p.Next
	}
}

func descending(from, to int) []int {
	var out []int
	for i := to; i >= from; i-- {
		out = append(out, i)
	}
	return out
}

func equal(a, b []int) bool {
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// TestRoundTrip verifies that records come back newest first, unflushed
// records are visible, and Seq is not stored.
func TestRoundTrip(t *testing.T) {
	l := open(t, t.TempDir(), Options{FlushInterval: time.Hour})
	appendN(t, l, 0, 10, "r")
	p, err := l.Read("", 3, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Records) != 3 || p.Records[0].Name != "q9.example" || p.Records[0].Seq != 0 || p.Next == "" {
		t.Fatalf("first page: %+v", p)
	}
	if !p.Records[0].Time.Equal(base.Add(9 * time.Second)) {
		t.Errorf("time: %v", p.Records[0].Time)
	}
	if got := readAll(t, l, 3, nil); !equal(got, descending(0, 9)) {
		t.Errorf("all: %v", got)
	}
}

// TestPagingAcrossSegments verifies that pages continue across segment
// boundaries without repeating or skipping a record, for several page sizes.
func TestPagingAcrossSegments(t *testing.T) {
	l := open(t, t.TempDir(), Options{SegmentBytes: 1000})
	appendN(t, l, 0, 200, "r")
	if st := l.Stats(); st.Files < 10 {
		t.Fatalf("expected many segments, got %d", st.Files)
	}
	for _, limit := range []int{1, 7, 50, 500} {
		if got := readAll(t, l, limit, nil); !equal(got, descending(0, 199)) {
			t.Errorf("limit %d: %v", limit, got)
		}
	}
}

// TestFilterAndScanBudget verifies that a filter is applied and that a scan
// that runs out of budget returns a cursor even with an empty page.
func TestFilterAndScanBudget(t *testing.T) {
	l := open(t, t.TempDir(), Options{SegmentBytes: 4000, ScanBytes: 2000})
	appendN(t, l, 0, 100, "other")
	appendN(t, l, 100, 1, "wanted")
	appendN(t, l, 101, 100, "other")
	keep := func(r *Record) bool { return r.Rule == "wanted" }
	p, err := l.Read("", 10, Filter{Keep: keep})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Records) != 0 || p.Next == "" {
		t.Fatalf("budget: want an empty page with a cursor, got %d records, next %q", len(p.Records), p.Next)
	}
	if got := readAll(t, l, 10, keep); !equal(got, []int{100}) {
		t.Errorf("filtered: %v", got)
	}
	// The raw-line prefilter alone selects the same record.
	var got []int
	for c := ""; ; {
		p, err := l.Read(c, 10, Filter{Contains: [][]byte{[]byte(`"rule":"wanted"`)}})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range p.Records {
			got = append(got, len(r.Name))
		}
		if c = p.Next; c == "" {
			break
		}
	}
	if len(got) != 1 {
		t.Errorf("prefilter: %v", got)
	}
}

// TestPrune verifies the size cap and that the current segment survives.
func TestPrune(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir, Options{SegmentBytes: 1000, MaxBytes: 3000})
	appendN(t, l, 0, 300, "r")
	l.prune(time.Now())
	st := l.Stats()
	if st.Bytes > 3000 || st.Files == 0 {
		t.Fatalf("after prune: %+v", st)
	}
	got := readAll(t, l, 100, nil)
	if len(got) == 0 || got[0] != 299 {
		t.Fatalf("newest records lost: %v", got)
	}
	if !st.Oldest.Equal(base.Add(time.Duration(got[len(got)-1]) * time.Second)) {
		t.Errorf("oldest %v, last read %d", st.Oldest, got[len(got)-1])
	}
}

// TestPruneAge verifies that segments older than MaxAge are removed.
func TestPruneAge(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir, Options{SegmentBytes: 1000, MaxAge: time.Hour})
	appendN(t, l, 0, 50, "r")
	old := time.Now().Add(-2 * time.Hour)
	names, _ := l.segments()
	for _, n := range names {
		if n != l.cur {
			_ = os.Chtimes(filepath.Join(dir, n), old, old)
		}
	}
	l.prune(time.Now())
	if st := l.Stats(); st.Files != 1 {
		t.Fatalf("want only the current segment, got %+v", st)
	}
}

// TestBadCursor verifies that a cursor cannot name a file outside the
// directory and that malformed cursors are rejected.
func TestBadCursor(t *testing.T) {
	l := open(t, t.TempDir(), Options{})
	appendN(t, l, 0, 3, "r")
	for _, c := range []string{"x", "../etc/passwd@0", "queries-x.jsonl@1", "queries-20261113T000000.000000000Z.jsonl@-1",
		"queries-20261113T000000.000000000Z.jsonl@abc", "/tmp/queries-20261113T000000.000000000Z.jsonl@0"} {
		if _, err := l.Read(c, 10, Filter{}); err != ErrBadCursor {
			t.Errorf("%q: %v", c, err)
		}
	}
	// A well-formed cursor for a pruned segment continues in the next older
	// segment; with none, the page is empty.
	p, err := l.Read("queries-19990101T000000.000000000Z.jsonl@5", 10, Filter{})
	if err != nil || len(p.Records) != 0 || p.Next != "" {
		t.Errorf("pruned cursor: %+v %v", p, err)
	}
}

// TestReopen verifies that a reopened log appends to the latest segment,
// repairs a partial last line, and keeps earlier records readable.
func TestReopen(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir, Options{})
	appendN(t, l, 0, 5, "r")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	names, _ := l.segments()
	f, err := os.OpenFile(filepath.Join(dir, names[len(names)-1]), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"time":"2026-11-13T00:00:0`) // crash mid-line
	_ = f.Close()

	l2 := open(t, dir, Options{})
	appendN(t, l2, 5, 5, "r")
	if got := readAll(t, l2, 4, nil); !equal(got, descending(0, 9)) {
		t.Errorf("after reopen: %v", got)
	}
	if n, _ := l2.segments(); len(n) != 1 {
		t.Errorf("want the segment reused, got %v", n)
	}
}

type countObs struct{ written, dropped, errs atomic.Int64 }

func (o *countObs) QueryLogWritten()    { o.written.Add(1) }
func (o *countObs) QueryLogDropped()    { o.dropped.Add(1) }
func (o *countObs) QueryLogWriteError() { o.errs.Add(1) }

// TestCloseFlushesAndDrops verifies that Close writes queued entries and that
// Append after Close is ignored.
func TestCloseFlushesAndDrops(t *testing.T) {
	dir := t.TempDir()
	obs := &countObs{}
	l, err := Open(dir, format, Options{MaxBytes: 1 << 20, MaxAge: time.Hour, FlushInterval: time.Hour, Observer: obs})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		l.Append(entry{n: i, at: base})
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l.Append(entry{n: 1000, at: base})
	names, _ := l.segments()
	data, _ := os.ReadFile(filepath.Join(dir, names[0]))
	if n := strings.Count(string(data), "\n"); n != 100 || obs.written.Load() != 100 {
		t.Errorf("lines %d, written %d", n, obs.written.Load())
	}
}

// TestOpenUnwritable verifies that Open reports a directory it cannot use.
func TestOpenUnwritable(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	_ = os.WriteFile(file, nil, 0o600)
	if _, err := Open(filepath.Join(file, "sub"), format, Options{MaxBytes: 1, MaxAge: 1}); err == nil {
		t.Fatal("want an error for a directory under a regular file")
	}
}

// TestRotateByAge verifies that a segment older than the rotation period is
// replaced on the next write, and is not reused when the log is reopened, so
// that MaxAge applies even at low query rates.
func TestRotateByAge(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir, Options{MaxAge: 4 * time.Hour}) // rotation period: 1h
	appendN(t, l, 0, 1, "r")
	l.mu.Lock()
	l.curAt = time.Now().Add(-2 * time.Hour)
	l.mu.Unlock()
	appendN(t, l, 1, 1, "r")
	if n, _ := l.segments(); len(n) != 2 {
		t.Fatalf("want a new segment after the rotation period, got %v", n)
	}
	_ = l.Close()

	// A newest segment whose name is older than the period is not reused.
	old := filepath.Join(dir, filePrefix+time.Now().Add(-2*time.Hour).UTC().Format(timeLayout)+fileSuffix)
	if err := os.WriteFile(old, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	for _, n := range must(l.segments()) {
		if filepath.Join(dir, n) != old {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
	l2 := open(t, dir, Options{MaxAge: 4 * time.Hour})
	if l2.cur == filepath.Base(old) {
		t.Fatal("reopened log appends to a segment older than the rotation period")
	}
}

// TestFailureBackoff verifies that after a failed write no new segment is
// opened until the retry delay has passed, and that lost records are counted.
func TestFailureBackoff(t *testing.T) {
	dir := t.TempDir()
	l := open(t, dir, Options{FlushInterval: time.Hour})
	appendN(t, l, 0, 1, "r")
	// Break the open file so that the next flush fails.
	l.mu.Lock()
	_ = l.f.Close()
	l.mu.Unlock()
	if _, err := l.Read("", 1, Filter{}); err != nil {
		t.Fatal(err)
	}
	before := must(l.segments())
	appendN(t, l, 1, 50, "r")
	if after := must(l.segments()); len(after) != len(before) {
		t.Fatalf("segments opened during the retry delay: %v -> %v", before, after)
	}
	if st := l.Stats(); st.Errors < 51 || st.LastError == "" {
		t.Fatalf("want the failure and every lost record counted, got %+v", st)
	}
	l.mu.Lock()
	l.retryAt = time.Time{}
	l.mu.Unlock()
	appendN(t, l, 51, 1, "r")
	if p, _ := l.Read("", 1, Filter{}); len(p.Records) != 1 || p.Records[0].Name != "q51.example" {
		t.Fatalf("no recovery after the retry delay: %+v", p)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
