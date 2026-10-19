package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"bitbucket.org/evoltth/evolt-ocpi-partner-console/internal/defaults"
)

type Store struct {
	db      *sql.DB
	aead    cipher.AEAD
	weakKey bool
	// env scopes every connection lookup; evoltVersionsURL seeds a new connection of that env.
	env              string
	evoltVersionsURL string
}

type Run struct {
	ID     string          `json:"id"`
	Name   string          `json:"name"`
	Status string          `json:"status"`
	Config json.RawMessage `json:"config"`
}

type Exchange struct {
	RunID           string
	Direction       string
	Module          string
	Method          string
	URL             string
	HTTPStatus      int
	RequestHeaders  any
	RequestBody     any
	ResponseHeaders any
	ResponseBody    any
	OCPIStatusCode  *int
	Findings        any
	Duration        time.Duration
}

func New(db *sql.DB, encryptionKey string) *Store {
	weak := encryptionKey == "" || strings.Contains(encryptionKey, "change-me") || strings.Contains(encryptionKey, "replace-with")
	if weak {
		encryptionKey = "ocpi-partner-mock-development-key-change-me"
	}
	key := sha256.Sum256([]byte(encryptionKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &Store{db: db, aead: aead, weakKey: weak, env: "dev"}
}

// ForEnv shares the database and key but only sees the connections of env.
func (s *Store) ForEnv(env, evoltVersionsURL string) *Store {
	scoped := *s
	scoped.env, scoped.evoltVersionsURL = env, evoltVersionsURL
	return &scoped
}

func (s *Store) WeakEncryptionKey() bool { return s.weakKey }

// Ping backs the readiness probe: the service is only ready when PostgreSQL answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) EnsureRun(ctx context.Context, defaultPublicURL string) (Run, error) {
	return s.EnsureRunProfile(ctx, "emsp", defaultPublicURL)
}

func (s *Store) EnsureRunProfile(ctx context.Context, profile, defaultPublicURL string) (Run, error) {
	run, err := s.CurrentRunProfile(ctx, profile)
	if err == nil {
		return run, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return run, err
	}
	run.ID = newID()
	run.Name = "Evolt black-box run"
	run.Status = "SETUP"
	config := s.defaultRunConfig(defaultPublicURL)
	body, _ := json.Marshal(config)
	_, err = s.db.ExecContext(ctx, `INSERT INTO partner_console_connections(id,leg,name,status,config,profile,env) VALUES($1,'emsp',$2,$3,$4,$5,$6)`, run.ID, run.Name, run.Status, body, profile, s.env)
	if err == nil {
		return s.CurrentRunProfile(ctx, profile)
	}
	run.Config = body
	return run, err
}

func (s *Store) ResetRun(ctx context.Context, runID, defaultPublicURL string) (Run, error) {
	return s.ResetRunProfile(ctx, runID, "emsp", defaultPublicURL)
}

func (s *Store) ResetRunProfile(ctx context.Context, runID, profile, defaultPublicURL string) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM partner_console_connections WHERE id=$1 AND leg='emsp'`, runID)
	if err != nil {
		return Run{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Run{}, sql.ErrNoRows
	}
	run := Run{ID: newID(), Name: "Evolt black-box run", Status: "SETUP"}
	run.Config, _ = json.Marshal(s.defaultRunConfig(defaultPublicURL))
	if _, err := tx.ExecContext(ctx, `INSERT INTO partner_console_connections(id,leg,name,status,config,profile,env) VALUES($1,'emsp',$2,$3,$4,$5,$6)`, run.ID, run.Name, run.Status, run.Config, profile, s.env); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (s *Store) defaultRunConfig(defaultPublicURL string) map[string]any {
	return map[string]any{
		"partner_public_url":      defaultPublicURL,
		"evolt_versions_url":      s.EvoltVersionsURL(),
		"country_code":            "TH",
		"party_id":                "TST",
		"business_name":           "OCPI Partner Mock",
		"strict":                  false,
		"raw_token_compatibility": false,
	}
}

// EvoltVersionsURL is the Versions URL a new connection of this env starts with.
func (s *Store) EvoltVersionsURL() string {
	if s.evoltVersionsURL != "" {
		return s.evoltVersionsURL
	}
	return defaults.EvoltVersionsURL()
}

func (s *Store) CurrentRun(ctx context.Context) (Run, error) {
	return s.CurrentRunProfile(ctx, "emsp")
}

func (s *Store) CurrentRunProfile(ctx context.Context, profile string) (Run, error) {
	var run Run
	err := s.db.QueryRowContext(ctx, `SELECT id, name, status, config FROM partner_console_connections WHERE leg='emsp' AND profile=$1 AND env=$2 ORDER BY created_at LIMIT 1`, profile, s.env).Scan(&run.ID, &run.Name, &run.Status, &run.Config)
	return run, err
}

func (s *Store) UpdateRun(ctx context.Context, id, name, status string, config any) error {
	body, err := json.Marshal(config)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE partner_console_connections SET name=$2,status=$3,config=$4,updated_at=now() WHERE id=$1`, id, name, status, body)
	return err
}

func (s *Store) UpdateRunAndDeleteSecret(ctx context.Context, id, name, status string, config any, secretKey string) error {
	body, err := json.Marshal(config)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE partner_console_connections SET name=$2,status=$3,config=$4,secrets=secrets-$5::text,updated_at=now() WHERE id=$1`, id, name, status, body, secretKey)
	return err
}

func (s *Store) PutSecret(ctx context.Context, runID, key, value string) error {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	ciphertext := s.aead.Seal(nonce, nonce, []byte(value), []byte(runID+":"+key))
	encoded := base64.RawStdEncoding.EncodeToString(ciphertext)
	result, err := s.db.ExecContext(ctx, `UPDATE partner_console_connections SET secrets=secrets||jsonb_build_object($2::text,jsonb_build_object('ciphertext',$3::text,'updated_at',now()::text)) WHERE id=$1`, runID, key, encoded)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) GetSecret(ctx context.Context, runID, key string) (string, error) {
	var encoded string
	if err := s.db.QueryRowContext(ctx, `SELECT secret.value->>'ciphertext' FROM partner_console_connections, jsonb_each(secrets) AS secret WHERE id=$1 AND secret.key=$2`, runID, key).Scan(&encoded); err != nil {
		return "", err
	}
	return s.decryptPartnerSecret(runID, key, encoded)
}

// decryptPartnerSecret reverses PutSecret. The additional data is runID:key, which is what keeps a
// ciphertext from being replayed under another run or another key — the CPO lab seals its own
// secrets with a different prefix, so the two stores can never decrypt each other's rows.
func (s *Store) decryptPartnerSecret(runID, key, encoded string) (string, error) {
	ciphertext, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(ciphertext) < s.aead.NonceSize() {
		return "", fmt.Errorf("invalid encrypted secret")
	}
	nonce, payload := ciphertext[:s.aead.NonceSize()], ciphertext[s.aead.NonceSize():]
	plain, err := s.aead.Open(nil, nonce, payload, []byte(runID+":"+key))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (s *Store) ReplaceEndpoints(ctx context.Context, runID, platform string, endpoints []map[string]any) error {
	entries := make([]map[string]any, 0, len(endpoints))
	for _, endpoint := range endpoints {
		entries = append(entries, map[string]any{"platform": platform, "identifier": text(endpoint["identifier"]), "role": text(endpoint["role"]), "url": text(endpoint["url"]), "raw": endpoint})
	}
	raw, _ := json.Marshal(entries)
	_, err := s.db.ExecContext(ctx, `UPDATE partner_console_connections SET endpoints=
		COALESCE((SELECT jsonb_agg(e ORDER BY n) FROM jsonb_array_elements(endpoints) WITH ORDINALITY AS t(e,n) WHERE e->>'platform'<>$2),'[]'::jsonb)
		|| COALESCE((SELECT jsonb_agg(e||jsonb_build_object('created_at',now()) ORDER BY n) FROM jsonb_array_elements($3::jsonb) WITH ORDINALITY AS t(e,n)),'[]'::jsonb)
		WHERE id=$1`, runID, platform, raw)
	return err
}

func (s *Store) Endpoint(ctx context.Context, runID, identifier, role string) (string, error) {
	var url string
	err := s.db.QueryRowContext(ctx, `SELECT e->>'url' FROM partner_console_connections, jsonb_array_elements(endpoints) WITH ORDINALITY AS t(e,n) WHERE id=$1 AND e->>'platform'='remote' AND e->>'identifier'=$2 AND ($3='' OR e->>'role'=$3) ORDER BY n LIMIT 1`, runID, identifier, role).Scan(&url)
	return url, err
}

func (s *Store) RemoteEndpoints(ctx context.Context, runID string) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e->>'identifier',e->>'role',e->>'url' FROM partner_console_connections, jsonb_array_elements(endpoints) WITH ORDINALITY AS t(e,n) WHERE id=$1 AND e->>'platform'='remote' ORDER BY n`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	endpoints := []map[string]any{}
	for rows.Next() {
		var identifier, role, url string
		if err := rows.Scan(&identifier, &role, &url); err != nil {
			return nil, err
		}
		endpoints = append(endpoints, map[string]any{"identifier": identifier, "role": role, "url": url})
	}
	return endpoints, rows.Err()
}

func (s *Store) UpsertToken(ctx context.Context, runID string, body map[string]any) error {
	updated, err := parseTime(body["last_updated"])
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(body)
	_, err = s.db.ExecContext(ctx, `INSERT INTO partner_console_objects(connection_id,leg,module,country_code,party_id,object_id,token_type,body,last_updated)
		VALUES($1,'emsp','tokens',$2,$3,$4,$5,$6,$7) ON CONFLICT(connection_id,module,country_code,party_id,object_id,token_type)
		DO UPDATE SET body=excluded.body,last_updated=excluded.last_updated,updated_at=now()`, runID, text(body["country_code"]), text(body["party_id"]), text(body["uid"]), text(body["type"]), raw, updated)
	return err
}

func (s *Store) Token(ctx context.Context, runID, uid, tokenType string) (map[string]any, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM partner_console_objects WHERE connection_id=$1 AND module='tokens' AND object_id=$2 AND token_type=$3`, runID, uid, tokenType).Scan(&raw)
	var body map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &body)
	}
	return body, err
}

func (s *Store) Tokens(ctx context.Context, runID string, dateFrom, dateTo *time.Time, offset, limit int) ([]map[string]any, int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body, count(*) OVER() FROM partner_console_objects WHERE connection_id=$1 AND module='tokens'
		AND ($2::timestamptz IS NULL OR last_updated >= $2) AND ($3::timestamptz IS NULL OR last_updated < $3)
		ORDER BY last_updated,object_id OFFSET $4 LIMIT $5`, runID, dateFrom, dateTo, offset, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []map[string]any{}
	total := 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw, &total); err != nil {
			return nil, 0, err
		}
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func (s *Store) PutObject(ctx context.Context, runID, module, countryCode, partyID, objectID string, body map[string]any, patch bool) error {
	if err := checkObjectModule(module); err != nil {
		return err
	}
	if module == "cdrs" && patch {
		return fmt.Errorf("CDRs are immutable")
	}
	var current map[string]any
	if patch {
		current, _ = s.Object(ctx, runID, module, countryCode, partyID, objectID)
		if current == nil {
			return sql.ErrNoRows
		}
		body = Merge(current, body, module == "sessions")
	}
	raw, _ := json.Marshal(body)
	updated, _ := parseTime(body["last_updated"])
	// CDRs are immutable, so a repeated id fails on the primary key instead of overwriting.
	if module == "cdrs" {
		_, err := s.db.ExecContext(ctx, `INSERT INTO partner_console_objects(connection_id,leg,module,country_code,party_id,object_id,body,last_updated) VALUES($1,'emsp',$2,$3,$4,$5,$6,$7)`, runID, module, countryCode, partyID, objectID, raw, nullableTime(updated))
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO partner_console_objects(connection_id,leg,module,country_code,party_id,object_id,body,last_updated) VALUES($1,'emsp',$2,$3,$4,$5,$6,$7)
		ON CONFLICT(connection_id,module,country_code,party_id,object_id,token_type) DO UPDATE SET body=excluded.body,last_updated=excluded.last_updated,updated_at=now()`, runID, module, countryCode, partyID, objectID, raw, nullableTime(updated))
	return err
}

func (s *Store) Object(ctx context.Context, runID, module, countryCode, partyID, objectID string) (map[string]any, error) {
	if err := checkObjectModule(module); err != nil {
		return nil, err
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM partner_console_objects WHERE connection_id=$1 AND module=$2 AND country_code=$3 AND party_id=$4 AND object_id=$5 AND token_type=''`, runID, module, countryCode, partyID, objectID).Scan(&raw)
	var body map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &body)
	}
	return body, err
}

func (s *Store) DeleteObject(ctx context.Context, runID, module, countryCode, partyID, objectID string) error {
	if err := checkObjectModule(module); err != nil {
		return err
	}
	if module != "tariffs" {
		return fmt.Errorf("delete unsupported for %s", module)
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM partner_console_objects WHERE connection_id=$1 AND module=$2 AND country_code=$3 AND party_id=$4 AND object_id=$5 AND token_type=''`, runID, module, countryCode, partyID, objectID)
	return err
}

func (s *Store) CreateCommand(ctx context.Context, id, runID, commandType, responseURL string, request any) error {
	raw, _ := json.Marshal(request)
	_, err := s.db.ExecContext(ctx, `INSERT INTO partner_console_commands(id,connection_id,leg,type,status,request,response_url) VALUES($1,$2,'emsp',$3,'SENT',$4,$5)`, id, runID, commandType, raw, responseURL)
	return err
}

func (s *Store) UpdateCommandDirect(ctx context.Context, runID, id, status string, timeout int, response any) error {
	raw, _ := json.Marshal(response)
	result, err := s.db.ExecContext(ctx, `UPDATE partner_console_commands SET status=$3,direct_response=$4,timeout_seconds=$5,updated_at=now() WHERE connection_id=$1 AND id=$2 AND leg='emsp'`, runID, id, status, raw, timeout)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) UpdateCommandAsync(ctx context.Context, runID, id, status string, response any) error {
	raw, _ := json.Marshal(response)
	result, err := s.db.ExecContext(ctx, `UPDATE partner_console_commands SET status=$3,async_result=$4,updated_at=now() WHERE connection_id=$1 AND id=$2 AND leg='emsp'`, runID, id, status, raw)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) AddExchange(ctx context.Context, exchange Exchange) error {
	reqHeaders, _ := json.Marshal(exchange.RequestHeaders)
	reqBody, _ := json.Marshal(exchange.RequestBody)
	resHeaders, _ := json.Marshal(exchange.ResponseHeaders)
	resBody, _ := json.Marshal(exchange.ResponseBody)
	findings, _ := json.Marshal(exchange.Findings)
	_, err := s.db.ExecContext(ctx, `INSERT INTO partner_console_exchanges(connection_id,direction,module,method,url,http_status,request_headers,request_body,response_headers,response_body,ocpi_status_code,findings,duration_ms)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, exchange.RunID, exchange.Direction, exchange.Module, exchange.Method, exchange.URL, exchange.HTTPStatus, reqHeaders, nullableJSON(reqBody), resHeaders, nullableJSON(resBody), exchange.OCPIStatusCode, findings, exchange.Duration.Milliseconds())
	return err
}

func (s *Store) State(ctx context.Context, runID string, revealSecrets bool) (map[string]json.RawMessage, error) {
	queries := map[string]string{
		"setup":     `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT id,name,status,config,created_at,updated_at FROM partner_console_connections WHERE id=$1) x`,
		"endpoints": `SELECT COALESCE(jsonb_agg(e-'raw' ORDER BY n), '[]'::jsonb) FROM partner_console_connections, jsonb_array_elements(endpoints) WITH ORDINALITY AS t(e,n) WHERE id=$1`,
		"tokens":    `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT country_code,party_id,object_id AS uid,token_type AS type,body,last_updated,updated_at FROM partner_console_objects WHERE connection_id=$1 AND module='tokens' ORDER BY updated_at DESC) x`,
		"locations": objectStateQuery("locations"),
		"tariffs":   objectStateQuery("tariffs"),
		"sessions":  objectStateQuery("sessions"),
		"cdrs":      `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT country_code,party_id,object_id,body,last_updated,created_at FROM partner_console_objects WHERE connection_id=$1 AND module='cdrs' ORDER BY created_at DESC) x`,
		"commands":  `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT id,connection_id,type,status,request,direct_response,async_result,response_url,timeout_seconds,created_at,updated_at FROM partner_console_commands WHERE connection_id=$1 ORDER BY created_at DESC LIMIT 100) x`,
		"exchanges": `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT seq AS id,connection_id,direction,module,method,url,http_status,request_headers,request_body,response_headers,response_body,ocpi_status_code,findings,duration_ms,created_at FROM partner_console_exchanges WHERE connection_id=$1 ORDER BY created_at DESC LIMIT 200) x`,
	}
	result := make(map[string]json.RawMessage, len(queries)+1)
	for name, query := range queries {
		var raw []byte
		if err := s.db.QueryRowContext(ctx, query, runID).Scan(&raw); err != nil {
			return nil, err
		}
		result[name] = raw
	}
	secrets, err := s.partnerSecretTable(ctx, runID, revealSecrets)
	if err != nil {
		return nil, err
	}
	result["secrets"] = secrets
	return result, nil
}

// partnerSecretTable is the Inbound half of what cpoLabSecretTable does for the CPO lab: the same
// row shape, the same fingerprints, decrypted only when the operator asked to see them. Both halves
// exist because the two runs are separate registrations with separate token pairs, and a rotation
// has to be provable on each of them on its own.
func objectStateQuery(module string) string {
	return `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT country_code,party_id,object_id,body,last_updated,updated_at FROM partner_console_objects WHERE connection_id=$1 AND module='` + module + `' ORDER BY updated_at DESC) x`
}

func (s *Store) partnerSecretTable(ctx context.Context, runID string, reveal bool) (json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT secret.key,secret.value->>'ciphertext',secret.value->>'updated_at' FROM partner_console_connections, jsonb_each(secrets) AS secret WHERE id=$1 ORDER BY secret.key`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var key, ciphertext, updatedAt string
		if err := rows.Scan(&key, &ciphertext, &updatedAt); err != nil {
			return nil, err
		}
		value := ""
		if reveal {
			value, err = s.decryptPartnerSecret(runID, key, ciphertext)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, cpoLabSecretRow(key, updatedAt, value, reveal))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func Merge(current, patch map[string]any, appendChargingPeriods bool) map[string]any {
	result := make(map[string]any, len(current)+len(patch))
	for key, value := range current {
		result[key] = value
	}
	for key, value := range patch {
		if appendChargingPeriods && key == "charging_periods" {
			oldList, _ := result[key].([]any)
			newList, _ := value.([]any)
			result[key] = append(append([]any{}, oldList...), newList...)
			continue
		}
		oldMap, oldOK := result[key].(map[string]any)
		newMap, newOK := value.(map[string]any)
		if oldOK && newOK {
			result[key] = Merge(oldMap, newMap, false)
		} else {
			result[key] = value
		}
	}
	return result
}

// checkObjectModule limits the eMSP receivers to the modules Evolt pushes as CPO.
func checkObjectModule(module string) error {
	switch module {
	case "locations", "tariffs", "sessions", "cdrs":
		return nil
	default:
		return fmt.Errorf("unsupported object module %q", module)
	}
}

func parseTime(value any) (time.Time, error) {
	if value == nil {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, text(value))
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func nullableJSON(raw []byte) any {
	if string(raw) == "null" {
		return nil
	}
	return raw
}

func text(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
