package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type RequestFixture struct {
	ID           string          `json:"id"`
	Lane         string          `json:"lane"`
	Module       string          `json:"module"`
	Name         string          `json:"name"`
	Content      string          `json:"content"`
	Method       string          `json:"method"`
	TargetURL    string          `json:"target_url"`
	Headers      json.RawMessage `json:"headers"`
	UseAuth      bool            `json:"use_auth"`
	ExpectedHTTP *int            `json:"expected_http,omitempty"`
	ExpectedOCPI *int            `json:"expected_ocpi,omitempty"`
	ValidJSON    bool            `json:"valid_json"`
	Findings     json.RawMessage `json:"findings"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

type RequestExchange struct {
	ID         int64           `json:"id"`
	FixtureID  string          `json:"fixture_id"`
	Request    json.RawMessage `json:"request"`
	Response   json.RawMessage `json:"response"`
	Status     string          `json:"status"`
	DurationMS int64           `json:"duration_ms"`
	CreatedAt  time.Time       `json:"created_at"`
}

func (s *Store) ListRequestFixtures(ctx context.Context) ([]RequestFixture, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,lane,module,name,content,method,target_url,headers,use_auth,expected_http,expected_ocpi,valid_json,findings,updated_at FROM request_fixtures ORDER BY lane,module,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var fixtures []RequestFixture
	for rows.Next() {
		var fixture RequestFixture
		if err := rows.Scan(&fixture.ID, &fixture.Lane, &fixture.Module, &fixture.Name, &fixture.Content, &fixture.Method, &fixture.TargetURL, &fixture.Headers, &fixture.UseAuth, &fixture.ExpectedHTTP, &fixture.ExpectedOCPI, &fixture.ValidJSON, &fixture.Findings, &fixture.UpdatedAt); err != nil {
			return nil, err
		}
		fixtures = append(fixtures, fixture)
	}
	return fixtures, rows.Err()
}

func (s *Store) UpsertRequestFixture(ctx context.Context, fixture RequestFixture) error {
	if len(fixture.Headers) == 0 {
		fixture.Headers = json.RawMessage(`{}`)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO request_fixtures(id,lane,module,name,content,method,target_url,headers,use_auth,expected_http,expected_ocpi,valid_json,findings) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT(id) DO UPDATE SET lane=excluded.lane,module=excluded.module,name=excluded.name,content=excluded.content,method=excluded.method,target_url=excluded.target_url,headers=excluded.headers,use_auth=excluded.use_auth,expected_http=excluded.expected_http,expected_ocpi=excluded.expected_ocpi,valid_json=excluded.valid_json,findings=excluded.findings,updated_at=now()`,
		fixture.ID, fixture.Lane, fixture.Module, fixture.Name, fixture.Content, fixture.Method, fixture.TargetURL, fixture.Headers, fixture.UseAuth, fixture.ExpectedHTTP, fixture.ExpectedOCPI, fixture.ValidJSON, fixture.Findings)
	return err
}

func (s *Store) GetRequestFixture(ctx context.Context, id string) (RequestFixture, error) {
	var fixture RequestFixture
	err := s.db.QueryRowContext(ctx, `SELECT id,lane,module,name,content,method,target_url,headers,use_auth,expected_http,expected_ocpi,valid_json,findings,updated_at FROM request_fixtures WHERE id=$1`, id).
		Scan(&fixture.ID, &fixture.Lane, &fixture.Module, &fixture.Name, &fixture.Content, &fixture.Method, &fixture.TargetURL, &fixture.Headers, &fixture.UseAuth, &fixture.ExpectedHTTP, &fixture.ExpectedOCPI, &fixture.ValidJSON, &fixture.Findings, &fixture.UpdatedAt)
	return fixture, err
}

func (s *Store) AddRequestExchange(ctx context.Context, exchange RequestExchange) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO request_exchanges(fixture_id,request,response,status,duration_ms) VALUES($1,$2,$3,$4,$5)`, exchange.FixtureID, exchange.Request, exchange.Response, exchange.Status, exchange.DurationMS)
	return err
}

func (s *Store) RecentRequestExchanges(ctx context.Context) ([]RequestExchange, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,fixture_id,request,response,status,duration_ms,created_at FROM request_exchanges ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var exchanges []RequestExchange
	for rows.Next() {
		var exchange RequestExchange
		if err := rows.Scan(&exchange.ID, &exchange.FixtureID, &exchange.Request, &exchange.Response, &exchange.Status, &exchange.DurationMS, &exchange.CreatedAt); err != nil {
			return nil, err
		}
		exchanges = append(exchanges, exchange)
	}
	return exchanges, rows.Err()
}

func (s *Store) LaneEndpointURLs(ctx context.Context, lane string) ([]string, error) {
	var rows *sql.Rows
	var err error
	switch lane {
	case "EMSP", "DUAL_EMSP":
		profile := "emsp"
		if lane == "DUAL_EMSP" {
			profile = "dual"
		}
		rows, err = s.db.QueryContext(ctx, `SELECT endpoint.url
			FROM emsp_endpoints endpoint
			JOIN emsp_connections run ON run.id=endpoint.connection_id
			WHERE endpoint.platform='remote' AND run.profile=$1
			ORDER BY run.updated_at DESC, endpoint.id`, profile)
	case "CPO", "DUAL_CPO":
		profile := "cpo"
		if lane == "DUAL_CPO" {
			profile = "dual"
		}
		rows, err = s.db.QueryContext(ctx, `SELECT endpoint.url
			FROM cpo_endpoints endpoint
			JOIN cpo_connections run ON run.id=endpoint.connection_id
			WHERE run.profile=$1
			ORDER BY run.updated_at DESC, endpoint.id`, profile)
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}
