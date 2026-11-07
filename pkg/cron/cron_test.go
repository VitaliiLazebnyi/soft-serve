package cron

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/robfig/cron/v3"
)

func TestCronLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := log.New(&buf)
	logger.SetLevel(log.DebugLevel)
	clogger := cronLogger{logger}
	clogger.Info("foo")
	clogger.Error(fmt.Errorf("bar"), "test")
	if buf.String() != "DEBUG foo\nERROR test err=bar\n" {
		t.Errorf("unexpected log output: %s", buf.String())
	}
}

func TestSchedularAddRemove(t *testing.T) {
	s := NewScheduler(context.TODO())
	id, err := s.AddFunc("* * * * *", func() {})
	if err != nil {
		t.Fatal(err)
	}
	s.Remove(id)
}

func TestSchedulerSkipsOverlappingRuns(t *testing.T) {
	s := NewScheduler(context.TODO())

	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	id, err := s.AddFunc("* * * * *", func() {
		runs.Add(1)
		started <- struct{}{}
		<-release
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Remove(id)

	job := s.Cron.Entry(cron.EntryID(id)).WrappedJob

	first := make(chan struct{})
	go func() {
		job.Run()
		close(first)
	}()
	<-started

	second := make(chan struct{})
	go func() {
		job.Run()
		close(second)
	}()

	select {
	case <-second:
	case <-started:
		t.Fatal("second run started while the first one was still running")
	case <-time.After(5 * time.Second):
		t.Fatal("second run did not return while the first one was running")
	}

	close(release)
	<-first

	if n := runs.Load(); n != 1 {
		t.Errorf("expected 1 run, got %d", n)
	}
}
