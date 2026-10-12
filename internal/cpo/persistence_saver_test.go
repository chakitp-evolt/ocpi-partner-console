package cpo

import (
	"context"
	"sync"
	"testing"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
)

type recordingStore struct {
	*store.Store
	mu      sync.Mutex
	events  []string
	onSave  func()
	release chan struct{}
}

func (s *recordingStore) SaveCPO(_ context.Context, snapshot store.CPOSnapshot) error {
	if s.onSave != nil {
		s.onSave()
	}
	if s.release != nil {
		<-s.release
	}
	s.record("save:" + snapshot.RunID)
	return nil
}

func (s *recordingStore) DeleteCPO(_ context.Context, runID string) error {
	s.record("delete:" + runID)
	return nil
}

func (s *recordingStore) record(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *recordingStore) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

func waitForEvents(t *testing.T, s *recordingStore, count int) []string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if events := s.snapshot(); len(events) >= count {
			return events
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected %d store events, got %v", count, s.snapshot())
	return nil
}

func newSavingController(repository *recordingStore, delay time.Duration) *Controller {
	c := New("http://mock.example")
	c.repository = repository
	c.saveDelay = delay
	c.startSaver()
	return c
}

func TestBurstOfChangesIsSavedOnce(t *testing.T) {
	repository := &recordingStore{}
	c := newSavingController(repository, 50*time.Millisecond)
	for range 20 {
		c.changed()
	}
	waitForEvents(t, repository, 1)
	time.Sleep(150 * time.Millisecond)
	if events := repository.snapshot(); len(events) != 1 {
		t.Fatalf("burst wrote the run %d times: %v", len(events), events)
	}
}

func TestSaveRunsOutsideStateLock(t *testing.T) {
	c := New("http://mock.example")
	repository := &recordingStore{}
	repository.onSave = func() {
		if !c.mu.TryLock() {
			t.Error("state lock is held during the database write")
			return
		}
		c.mu.Unlock()
	}
	c.repository = repository
	c.changed()
	waitForEvents(t, repository, 1)
}

func TestFlushWritesPendingChange(t *testing.T) {
	repository := &recordingStore{}
	c := newSavingController(repository, time.Hour)
	c.changed()
	if err := c.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if events := repository.snapshot(); len(events) != 1 || events[0] != "save:"+c.state.RunID {
		t.Fatalf("flush did not save the pending run: %v", events)
	}
}

func TestResetWaitsForInFlightSave(t *testing.T) {
	entered := make(chan struct{})
	var enteredOnce sync.Once
	repository := &recordingStore{release: make(chan struct{})}
	repository.onSave = func() { enteredOnce.Do(func() { close(entered) }) }
	c := newSavingController(repository, time.Millisecond)
	oldRunID := c.state.RunID
	c.changed()
	<-entered

	done := make(chan error, 1)
	go func() { done <- c.ResetData(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	if events := repository.snapshot(); len(events) != 0 {
		t.Fatalf("reset ran while the old run was still being written: %v", events)
	}
	close(repository.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	events := waitForEvents(t, repository, 3)
	c.mu.RLock()
	newRunID := c.state.RunID
	c.mu.RUnlock()
	want := []string{"save:" + oldRunID, "delete:" + oldRunID, "save:" + newRunID}
	for i, event := range want {
		if events[i] != event {
			t.Fatalf("store events = %v, want %v", events, want)
		}
	}
}
