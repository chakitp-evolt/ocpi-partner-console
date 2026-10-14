package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

type shutdownServer struct {
	err    error
	ctx    context.Context
	closed bool
}

func (s *shutdownServer) Shutdown(ctx context.Context) error {
	s.ctx = ctx
	return s.err
}

func (s *shutdownServer) Close() error {
	s.closed = true
	return nil
}

type shutdownConsole struct {
	t      *testing.T
	server *shutdownServer
	called bool
}

func (c *shutdownConsole) Close(ctx context.Context) error {
	c.called = true
	if err := ctx.Err(); err != nil {
		c.t.Fatalf("database flush received an expired context: %v", err)
	}
	if c.server.ctx.Err() == nil {
		c.t.Fatal("HTTP shutdown context was not released before the database flush")
	}
	if _, ok := ctx.Deadline(); !ok {
		c.t.Fatal("database flush has no deadline")
	}
	return nil
}

func TestShutdownGivesDatabaseFlushAnIndependentDeadline(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "HTTP drained"},
		{name: "SSE exhausted HTTP deadline", err: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &shutdownServer{err: tc.err}
			console := &shutdownConsole{t: t, server: server}
			shutdown(server, console, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if !console.called {
				t.Fatal("pending partner state was not flushed")
			}
			if server.closed != (tc.err != nil) {
				t.Fatal("HTTP connections must be forced closed only when draining fails")
			}
		})
	}
}
