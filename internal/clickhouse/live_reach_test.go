package clickhouse

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coltconsulting/click-dog/internal/clicklog"
)

// captureWarns routes clicklog warnings into a buffer for the test.
func captureWarns(t *testing.T) *bytes.Buffer {
	t.Helper()
	if err := clicklog.InitLogger("warn", "", "text", 0, 0); err != nil {
		t.Fatalf("InitLogger: %v", err)
	}
	t.Cleanup(clicklog.CloseLogger)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// TestFetchLiveSpans_ReportsRowsBehindTheRetryReach drives a collector outage
// longer than the retry reach. The walk resumes at the oldest row it can still
// read, and the rows between where it last read and that floor are lost by
// design; each stretch of lost time must be named in one warning, and a walk
// back inside its reach must warn no more. The last delivered fetch reached
// the head at 20s, so everything that settled by 5s was read, past the
// watermark at 1s. A host clock behind the server's, within the slack, gives
// the same ranges.
func TestFetchLiveSpans_ReportsRowsBehindTheRetryReach(t *testing.T) {
	const lookback = 40 * time.Second // reach: 3 x 40s + the 15s settle = 135s
	for _, skew := range []time.Duration{0, 20 * time.Second} {
		t.Run(fmt.Sprintf("host clock %s behind", skew), func(t *testing.T) {
			buf := captureWarns(t)
			l, reader := newLiveSpanLog(t)
			reader.now = func() time.Time { return time.UnixMicro(int64(l.nowUs)).Add(-skew) }
			l.add(testTraceID(1), 1, seconds(1), 0)
			l.add(testTraceID(2), 1, seconds(30), 0)
			l.add(testTraceID(3), 1, seconds(200), 0)
			fetch := func(now float64) {
				t.Helper()
				l.nowUs = seconds(now)
				if _, err := reader.FetchLiveSpans(context.Background(), 1, lookback, 100, FetchOpts{}); err != nil {
					t.Fatalf("FetchLiveSpans at %.0fs: %v", now, err)
				}
			}

			fetch(20)
			reader.CommitLiveSpans() // delivered the trace at 1s and reached the head
			if buf.Len() != 0 {
				t.Fatalf("healthy fetch warned:\n%s", buf)
			}

			// The collector is down from here: no fetch commits.
			fetch(150)
			if got := buf.String(); !strings.Contains(got, "span-log rows that finished from 1970-01-01T00:00:05Z to 1970-01-01T00:00:15Z can no longer be read") {
				t.Fatalf("first fetch past the reach = %q, want the 5s-15s range named", got)
			}
			buf.Reset()

			fetch(300)
			if got := buf.String(); !strings.Contains(got, "from 1970-01-01T00:00:15Z to 1970-01-01T00:02:45Z") {
				t.Fatalf("later fetch = %q, want only the newly unreadable 15s-165s range", got)
			}
			buf.Reset()

			fetch(300)
			if buf.Len() != 0 {
				t.Fatalf("a range already reported was reported again:\n%s", buf)
			}

			// The collector is back: the walk delivers the trace at 200s and
			// is inside its reach again.
			reader.CommitLiveSpans()
			reads := l.serverTimeReads
			fetch(330)
			if buf.Len() != 0 {
				t.Fatalf("a walk back inside its reach warned:\n%s", buf)
			}
			if l.serverTimeReads != reads {
				t.Errorf("a walk back inside its reach read the server clock")
			}
		})
	}
}

// TestFetchLiveSpans_QuietSpanLogIsNotALoss pins the false alarm release review
// found: one trace delivered, then nothing qualifying for minutes. The
// watermark stays at that trace while every fetch reads up to the head, so
// nothing is behind the reach. No warning may fire and the server's clock is
// not read.
func TestFetchLiveSpans_QuietSpanLogIsNotALoss(t *testing.T) {
	buf := captureWarns(t)
	l, reader := newLiveSpanLog(t)
	l.add(testTraceID(1), 1, seconds(1), 0)
	for now := 20.0; now <= 600; now += 30 {
		l.nowUs = seconds(now)
		if _, err := reader.FetchLiveSpans(context.Background(), 1, 40*time.Second, 100, FetchOpts{}); err != nil {
			t.Fatalf("FetchLiveSpans at %.0fs: %v", now, err)
		}
		reader.CommitLiveSpans()
	}
	if buf.Len() != 0 {
		t.Errorf("a quiet span log was reported as lost:\n%s", buf)
	}
	if l.serverTimeReads != 0 {
		t.Errorf("server clock reads = %d, want 0 on a quiet span log", l.serverTimeReads)
	}
}

// TestFetchLiveSpans_OutageAfterAQuietSpellNamesOnlyUnreadTime pins where the
// reported range starts after a quiet spell: at the settle before the last
// delivered fetch that reached the head, not at the old watermark.
func TestFetchLiveSpans_OutageAfterAQuietSpellNamesOnlyUnreadTime(t *testing.T) {
	buf := captureWarns(t)
	l, reader := newLiveSpanLog(t)
	l.add(testTraceID(1), 1, seconds(1), 0)
	l.add(testTraceID(2), 1, seconds(400), 0)
	fetch := func(now float64) {
		t.Helper()
		l.nowUs = seconds(now)
		if _, err := reader.FetchLiveSpans(context.Background(), 1, 40*time.Second, 100, FetchOpts{}); err != nil {
			t.Fatalf("FetchLiveSpans at %.0fs: %v", now, err)
		}
	}
	for now := 20.0; now <= 380; now += 30 {
		fetch(now)
		reader.CommitLiveSpans()
	}
	// The last delivered fetch reached the head at 380s. The collector then
	// fails until 560s, when the reach floor is 425s.
	fetch(560)
	if got := buf.String(); !strings.Contains(got, "from 1970-01-01T00:06:05Z to 1970-01-01T00:07:05Z") {
		t.Fatalf("warning = %q, want the range from 365s (380s less the settle) to 425s", got)
	}
}

// TestFetchLiveSpans_HealthyWalkDoesNotReadServerClock pins that the reach
// check costs a healthy walk nothing: when this host's clock puts the resume
// position well inside the window, the server's clock is not read.
func TestFetchLiveSpans_HealthyWalkDoesNotReadServerClock(t *testing.T) {
	l, reader := newLiveSpanLog(t)
	base := uint64(time.Now().Add(-30 * time.Second).UnixMicro())
	l.add(testTraceID(1), 1, base+seconds(1), 0)
	for _, now := range []uint64{base + seconds(20), base + seconds(30)} {
		l.nowUs = now
		if _, err := reader.FetchLiveSpans(context.Background(), 1, 40*time.Second, 100, FetchOpts{}); err != nil {
			t.Fatalf("FetchLiveSpans: %v", err)
		}
		reader.CommitLiveSpans()
	}
	if l.serverTimeReads != 0 {
		t.Errorf("server clock reads = %d, want 0 for a walk inside its reach", l.serverTimeReads)
	}
}
