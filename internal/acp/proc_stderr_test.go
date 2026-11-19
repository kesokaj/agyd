package acp

import (
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDrainStderrHandlesOversizedLineWithoutDeadlock(t *testing.T) {
	runPipe := func(t *testing.T, level slog.Level, payload string) *syncBuffer {
		t.Helper()
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}

		logs := &syncBuffer{}
		var lv slog.LevelVar
		lv.Set(level)
		p := &folderProc{
			client: &Client{log: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: &lv}))},
			dir:    "/ws/big-stderr",
		}

		drainDone := make(chan struct{})
		go p.drainStderr(pr, drainDone)

		writeDone := make(chan error, 1)
		go func() {
			_, werr := pw.WriteString(payload)
			cerr := pw.Close()
			if werr == nil {
				werr = cerr
			}
			writeDone <- werr
		}()

		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()

		var writeErr error
		select {
		case writeErr = <-writeDone:
		case <-timer.C:
			_ = pr.Close()
			_ = pw.Close()
			<-writeDone
			<-drainDone
			t.Fatalf("stderr writer blocked for >2s (pipe deadlock at level %s)", level)
		}

		select {
		case <-drainDone:
		case <-timer.C:
			_ = pr.Close()
			<-drainDone
			t.Fatalf("drainStderr did not finish within 2s at level %s", level)
		}
		_ = pr.Close()

		if writeErr != nil {
			t.Fatalf("writing stderr payload failed: %v", writeErr)
		}
		return logs
	}

	t.Run("DEBUG caps 256 KiB line at 4096 bytes with truncated flag and keeps reading", func(t *testing.T) {
		payload := strings.Repeat("x", 256*1024) + "\n" + "after-big-line\n"
		logs := runPipe(t, slog.LevelDebug, payload)

		recs := logs.records(t, "acp stderr")
		if len(recs) != 2 {
			t.Fatalf("got %d stderr records, want 2: %v", len(recs), recs)
		}
		line0, _ := recs[0]["line"].(string)
		if recs[0]["level"] != "DEBUG" || recs[0]["dir"] != "/ws/big-stderr" || len(line0) != 4096 || line0 != strings.Repeat("x", 4096) || recs[0]["truncated"] != true {
			t.Errorf("record[0] = level=%v dir=%v len=%d truncated=%v, want DEBUG /ws/big-stderr len=4096 truncated=true", recs[0]["level"], recs[0]["dir"], len(line0), recs[0]["truncated"])
		}
		if _, hasTrunc := recs[1]["truncated"]; hasTrunc {
			t.Errorf("record[1] unexpectedly set truncated: %v", recs[1])
		}
		if recs[1]["level"] != "DEBUG" || recs[1]["dir"] != "/ws/big-stderr" || recs[1]["line"] != "after-big-line" {
			t.Errorf("record[1] = %v, want DEBUG /ws/big-stderr line=after-big-line", recs[1])
		}
	})

	t.Run("INFO drains 256 KiB line completely with zero logs emitted", func(t *testing.T) {
		payload := strings.Repeat("y", 256*1024) + "\n" + "after-big-line\n"
		logs := runPipe(t, slog.LevelInfo, payload)

		if recs := logs.records(t, "acp stderr"); len(recs) != 0 {
			t.Fatalf("INFO emitted %d stderr records, want 0: %v", len(recs), recs)
		}
	})

	t.Run("DEBUG preserves CRLF, exact 4 KiB boundary, and final line at EOF", func(t *testing.T) {
		exactA := strings.Repeat("a", 4096)
		exactB := strings.Repeat("b", 4096)
		overC := strings.Repeat("c", 4097)
		payload := "crlf line\r\n" + exactA + "\n" + exactB + "\r\n" + overC + "\n" + "final-without-newline"
		logs := runPipe(t, slog.LevelDebug, payload)

		recs := logs.records(t, "acp stderr")
		want := []struct {
			line      string
			truncated bool
		}{
			{"crlf line", false},
			{exactA, false},
			{exactB, false},
			{strings.Repeat("c", 4096), true},
			{"final-without-newline", false},
		}
		if len(recs) != len(want) {
			t.Fatalf("got %d stderr records, want %d", len(recs), len(want))
		}
		for i, w := range want {
			gotLine, _ := recs[i]["line"].(string)
			gotTrunc, hasTrunc := recs[i]["truncated"].(bool)
			if gotLine != w.line || gotTrunc != w.truncated || (hasTrunc != w.truncated) {
				t.Errorf("record[%d]: len(line)=%d trunc=%v (present=%v), want len=%d trunc=%v", i, len(gotLine), gotTrunc, hasTrunc, len(w.line), w.truncated)
			}
		}
	})
}
