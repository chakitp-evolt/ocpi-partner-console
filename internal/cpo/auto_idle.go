package cpo

import (
	"context"
	"fmt"
	"time"
)

func (c *Controller) startIdleRun(sessionID, commandID string, unplugAt time.Time) {
	c.autoMu.Lock()
	if c.idleRuns[sessionID] != nil {
		c.autoMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := &autoMeterRun{cancel: cancel, done: make(chan struct{})}
	c.idleRuns[sessionID] = run
	c.autoMu.Unlock()
	go func() {
		defer close(run.done)
		defer func() {
			c.autoMu.Lock()
			if c.idleRuns[sessionID] == run {
				delete(c.idleRuns, sessionID)
			}
			c.autoMu.Unlock()
		}()
		timer := time.NewTimer(time.Until(unplugAt))
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		c.mu.RLock()
		record := c.state.Objects["sessions"][sessionID]
		ready := record != nil && fieldText(record.Body, "status") == "ACTIVE" && c.idleCommandLocked(sessionID) != nil && !c.unplugging[sessionID]
		c.mu.RUnlock()
		if !ready {
			return
		}
		if _, err := c.callSessionAction(ctx, "unplug", map[string]any{"session_id": sessionID}); err != nil {
			c.mu.RLock()
			busy := c.unplugging[sessionID]
			c.mu.RUnlock()
			if !busy {
				c.recordAutoError(commandID, fmt.Errorf("scheduled unplug failed: %w", err))
			}
		}
	}()
}

func (c *Controller) cancelIdleRun(sessionID string) {
	c.autoMu.Lock()
	if run := c.idleRuns[sessionID]; run != nil {
		run.cancel()
	}
	c.autoMu.Unlock()
}

func (c *Controller) stopAllIdleRuns() {
	c.autoMu.Lock()
	runs := make([]*autoMeterRun, 0, len(c.idleRuns))
	for _, run := range c.idleRuns {
		run.cancel()
		runs = append(runs, run)
	}
	c.autoMu.Unlock()
	for _, run := range runs {
		<-run.done
	}
}

func (c *Controller) resumeIdleRuns() {
	type pending struct {
		sessionID, commandID string
		at                   time.Time
	}
	var jobs []pending
	c.mu.RLock()
	for _, command := range c.state.Commands {
		if command.Type != "STOP_SESSION" || fieldText(command.Result, "idle_finished_at") != "" {
			continue
		}
		sessionID := fieldText(command.Request, "session_id")
		record := c.state.Objects["sessions"][sessionID]
		at, err := parseOCPITime(fieldText(command.Result, "auto_unplug_at"))
		if err == nil && record != nil && fieldText(record.Body, "status") == "ACTIVE" && fieldText(command.Result, "idle_started_at") != "" {
			jobs = append(jobs, pending{sessionID, command.ID, at})
		}
	}
	c.mu.RUnlock()
	for _, job := range jobs {
		c.startIdleRun(job.sessionID, job.commandID, job.at)
	}
}
