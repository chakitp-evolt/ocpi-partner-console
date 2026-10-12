package cpo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/defaults"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/ocpi"
	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/store"
)

type persistence interface {
	LoadCPO(context.Context) (store.CPOSnapshot, error)
	SaveCPO(context.Context, store.CPOSnapshot) error
	CPOTables(context.Context, string, bool) (map[string]json.RawMessage, error)
	DeleteCPO(context.Context, string) error
	FavoriteTokens(context.Context) ([]store.FavoriteToken, error)
	SaveFavoriteToken(context.Context, store.FavoriteToken) error
	DeleteFavoriteToken(context.Context, string) error
}

func NewPersistent(repository persistence, defaultPublicURL string) (*Controller, error) {
	return newPersistent(repository, defaultPublicURL, "cpo")
}

func NewPersistentProfile(repository persistence, defaultPublicURL, profile string) (*Controller, error) {
	return newPersistent(repository, defaultPublicURL, profile)
}

func newPersistent(repository persistence, defaultPublicURL, profile string) (*Controller, error) {
	ctx := context.Background()
	controller := New(defaultPublicURL)
	controller.profile = profile
	controller.repository = repository
	controller.startSaver()
	favorites, err := repository.FavoriteTokens(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range favorites {
		if profile == "dual" {
			if !strings.HasPrefix(item.ID, "dual:") {
				continue
			}
			item.ID = strings.TrimPrefix(item.ID, "dual:")
		} else if strings.HasPrefix(item.ID, "dual:") {
			continue
		}
		controller.favorites[item.ID] = &FavoriteToken{
			ID: item.ID, Label: item.Label, Token: item.Token,
			LocationID: item.LocationID, EVSEUID: item.EVSEUID, ConnectorID: item.ConnectorID, CreatedAt: item.CreatedAt,
		}
	}
	var snapshot store.CPOSnapshot
	if loader, ok := repository.(interface {
		LoadCPOProfile(context.Context, string) (store.CPOSnapshot, error)
	}); ok {
		snapshot, err = loader.LoadCPOProfile(ctx, profile)
	} else {
		snapshot, err = repository.LoadCPO(ctx)
	}
	if err == nil {
		if err := controller.restore(snapshot); err != nil {
			return nil, err
		}
		controller.resumeAutoRuns()
		controller.resumeIdleRuns()
		return controller, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err := controller.persist(ctx); err != nil {
		return nil, err
	}
	return controller, nil
}

const (
	defaultSaveDelay = time.Second
	saveTimeout      = 30 * time.Second
)

func (c *Controller) startSaver() {
	c.saves = make(chan struct{}, 1)
	c.stopSaving = make(chan struct{})
	c.savingDone = make(chan struct{})
	go c.saveLoop()
}

// scheduleSave queues a write; without the background writer it saves inline.
func (c *Controller) scheduleSave() {
	if c.repository == nil {
		return
	}
	if c.saves == nil {
		_ = c.save(context.Background())
		return
	}
	select {
	case c.saves <- struct{}{}:
	default:
	}
}

// saveLoop collapses every change inside saveDelay into one full rewrite of the run.
func (c *Controller) saveLoop() {
	defer close(c.savingDone)
	for {
		select {
		case <-c.stopSaving:
			return
		case <-c.saves:
		}
		select {
		case <-c.stopSaving:
			return
		case <-time.After(c.saveDelay):
		}
		select {
		case <-c.saves:
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), saveTimeout)
		_ = c.save(ctx)
		cancel()
	}
}

// Flush stops the background writer and saves the latest state.
func (c *Controller) Flush(ctx context.Context) error {
	if c.repository == nil {
		return nil
	}
	if c.saves != nil {
		c.stopSavingOnce.Do(func() { close(c.stopSaving) })
		select {
		case <-c.savingDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c.save(ctx)
}

func (c *Controller) save(ctx context.Context) error {
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	err := c.persist(ctx)
	message := ""
	if err != nil {
		message = err.Error()
	}
	c.mu.Lock()
	changed := c.persistenceError != message
	c.persistenceError = message
	c.mu.Unlock()
	if changed {
		c.notify()
	}
	return err
}

// persist writes outside c.mu so requests are not held behind the database round trip.
func (c *Controller) persist(ctx context.Context) error {
	return c.repository.SaveCPO(ctx, c.snapshot())
}

func (c *Controller) snapshot() store.CPOSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	config, _ := json.Marshal(c.state.Config)
	credentials, _ := json.Marshal(c.state.Credentials)
	snapshot := store.CPOSnapshot{
		RunID: c.state.RunID, Profile: c.profile, Status: c.state.Status, Config: config, Credentials: credentials,
		Secrets: map[string]string{"evolt_token_a": c.state.TokenA, "partner_initial_token_a": c.state.InitialToken, "partner_issued_token": c.state.TokenB, "evolt_issued_token": c.state.TokenC},
	}
	for _, endpoint := range c.state.Endpoints {
		raw, _ := json.Marshal(endpoint)
		snapshot.Endpoints = append(snapshot.Endpoints, raw)
	}
	for module, records := range c.state.Objects {
		for id, record := range records {
			body, _ := json.Marshal(record.Body)
			metadata, _ := json.Marshal(record.Snapshot)
			if record.Snapshot == nil {
				metadata = []byte("{}")
			}
			findings, _ := json.Marshal(record.Findings)
			snapshot.Objects = append(snapshot.Objects, store.CPOObject{Module: module, ObjectID: id, Body: body, Snapshot: metadata, Delivery: record.Delivery, Findings: findings, CDRLocation: record.CDRLocation, UpdatedAt: record.UpdatedAt})
		}
	}
	for _, command := range c.state.Commands {
		request, _ := json.Marshal(command.Request)
		result, _ := json.Marshal(command.Result)
		snapshot.Commands = append(snapshot.Commands, store.CPOCommand{ID: command.ID, Type: command.Type, Status: command.Status, Request: request, ResponseURL: command.ResponseURL, Result: result, CreatedAt: command.CreatedAt, UpdatedAt: command.UpdatedAt})
	}
	for _, exchange := range c.state.Exchanges {
		raw, _ := json.Marshal(exchange)
		snapshot.Exchanges = append(snapshot.Exchanges, raw)
	}
	return snapshot
}

func (c *Controller) restore(snapshot store.CPOSnapshot) error {
	state := memoryState{RunID: snapshot.RunID, Status: snapshot.Status, Objects: emptyObjects(), Commands: map[string]*CommandRecord{}, Endpoints: []Endpoint{}, Exchanges: []Exchange{}, Credentials: map[string]any{}}
	if err := json.Unmarshal(snapshot.Config, &state.Config); err != nil {
		return err
	}
	// A run saved before these defaults existed keeps its own values; only blanks are filled.
	if state.Config.EvoltVersionsURL == "" {
		state.Config.EvoltVersionsURL = defaults.EvoltVersionsURL()
	}
	if state.Config.PartnerPublicURL == "" {
		state.Config.PartnerPublicURL = c.defaultPublicURL
	}
	if state.Config.AutoMeterRateKWh == 0 {
		state.Config.AutoMeterRateKWh = 7.2
	}
	_ = json.Unmarshal(snapshot.Credentials, &state.Credentials)
	state.TokenA = snapshot.Secrets["evolt_token_a"]
	state.InitialToken = snapshot.Secrets["partner_initial_token_a"]
	state.TokenB = snapshot.Secrets["partner_issued_token"]
	state.TokenC = snapshot.Secrets["evolt_issued_token"]
	for _, raw := range snapshot.Endpoints {
		var endpoint Endpoint
		if err := json.Unmarshal(raw, &endpoint); err != nil {
			return err
		}
		state.Endpoints = append(state.Endpoints, endpoint)
	}
	for _, item := range snapshot.Objects {
		var body map[string]any
		var metadata map[string]any
		var findings []ocpi.Finding
		if err := json.Unmarshal(item.Body, &body); err != nil {
			return err
		}
		_ = json.Unmarshal(item.Findings, &findings)
		_ = json.Unmarshal(item.Snapshot, &metadata)
		if state.Objects[item.Module] == nil {
			state.Objects[item.Module] = map[string]*Record{}
		}
		state.Objects[item.Module][item.ObjectID] = &Record{Body: body, Snapshot: metadata, Delivery: item.Delivery, Findings: findings, CDRLocation: item.CDRLocation, UpdatedAt: item.UpdatedAt}
	}
	for _, item := range snapshot.Commands {
		var request, result map[string]any
		_ = json.Unmarshal(item.Request, &request)
		_ = json.Unmarshal(item.Result, &result)
		state.Commands[item.ID] = &CommandRecord{ID: item.ID, Type: item.Type, Status: item.Status, Request: request, ResponseURL: item.ResponseURL, Result: result, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
	}
	for _, raw := range snapshot.Exchanges {
		var exchange Exchange
		if err := json.Unmarshal(raw, &exchange); err != nil {
			return err
		}
		state.Exchanges = append(state.Exchanges, exchange)
	}
	c.mu.Lock()
	c.state = state
	c.mu.Unlock()
	return nil
}

func commandList(commands map[string]*CommandRecord) []*CommandRecord {
	result := make([]*CommandRecord, 0, len(commands))
	for _, command := range commands {
		result = append(result, command)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt > result[j].CreatedAt })
	return result
}

func storageMode(repository persistence) string {
	if repository == nil {
		return "memory-test"
	}
	return "postgresql"
}
