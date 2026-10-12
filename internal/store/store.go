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
	return &Store{db: db, aead: aead, weakKey: weak}
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
	config := defaultRunConfig(defaultPublicURL)
	body, _ := json.Marshal(config)
	_, err = s.db.ExecContext(ctx, `INSERT INTO emsp_connections(id,name,status,config,profile) VALUES($1,$2,$3,$4,$5)`, run.ID, run.Name, run.Status, body, profile)
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
	result, err := tx.ExecContext(ctx, `DELETE FROM emsp_connections WHERE id=$1`, runID)
	if err != nil {
		return Run{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return Run{}, sql.ErrNoRows
	}
	run := Run{ID: newID(), Name: "Evolt black-box run", Status: "SETUP"}
	run.Config, _ = json.Marshal(defaultRunConfig(defaultPublicURL))
	if _, err := tx.ExecContext(ctx, `INSERT INTO emsp_connections(id,name,status,config,profile) VALUES($1,$2,$3,$4,$5)`, run.ID, run.Name, run.Status, run.Config, profile); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	return run, nil
}

func defaultRunConfig(defaultPublicURL string) map[string]any {
	return map[string]any{
		"partner_public_url":      defaultPublicURL,
		"evolt_versions_url":      defaults.EvoltVersionsURL(),
		"country_code":            "TH",
		"party_id":                "TST",
		"business_name":           "OCPI Partner Mock",
		"strict":                  false,
		"raw_token_compatibility": false,
	}
}

func (s *Store) CurrentRun(ctx context.Context) (Run, error) {
	return s.CurrentRunProfile(ctx, "emsp")
}

func (s *Store) CurrentRunProfile(ctx context.Context, profile string) (Run, error) {
	var run Run
	err := s.db.QueryRowContext(ctx, `SELECT id, name, status, config FROM emsp_connections WHERE profile=$1 ORDER BY created_at LIMIT 1`, profile).Scan(&run.ID, &run.Name, &run.Status, &run.Config)
	return run, err
}

func (s *Store) UpdateRun(ctx context.Context, id, name, status string, config any) error {
	body, err := json.Marshal(config)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE emsp_connections SET name=$2,status=$3,config=$4,updated_at=now() WHERE id=$1`, id, name, status, body)
	return err
}

func (s *Store) UpdateRunAndDeleteSecret(ctx context.Context, id, name, status string, config any, secretKey string) error {
	body, err := json.Marshal(config)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE emsp_connections SET name=$2,status=$3,config=$4,updated_at=now() WHERE id=$1`, id, name, status, body); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM emsp_secrets WHERE connection_id=$1 AND key=$2`, id, secretKey); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PutSecret(ctx context.Context, runID, key, value string) error {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	ciphertext := s.aead.Seal(nonce, nonce, []byte(value), []byte(runID+":"+key))
	encoded := base64.RawStdEncoding.EncodeToString(ciphertext)
	_, err := s.db.ExecContext(ctx, `INSERT INTO emsp_secrets(connection_id,key,ciphertext) VALUES($1,$2,$3)
		ON CONFLICT(connection_id,key) DO UPDATE SET ciphertext=excluded.ciphertext,updated_at=now()`, runID, key, encoded)
	return err
}

func (s *Store) GetSecret(ctx context.Context, runID, key string) (string, error) {
	var encoded string
	if err := s.db.QueryRowContext(ctx, `SELECT ciphertext FROM emsp_secrets WHERE connection_id=$1 AND key=$2`, runID, key).Scan(&encoded); err != nil {
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM emsp_endpoints WHERE connection_id=$1 AND platform=$2`, runID, platform); err != nil {
		return err
	}
	for _, endpoint := range endpoints {
		raw, _ := json.Marshal(endpoint)
		if _, err := tx.ExecContext(ctx, `INSERT INTO emsp_endpoints(connection_id,platform,identifier,role,url,raw) VALUES($1,$2,$3,$4,$5,$6)`, runID, platform, text(endpoint["identifier"]), text(endpoint["role"]), text(endpoint["url"]), raw); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) Endpoint(ctx context.Context, runID, identifier, role string) (string, error) {
	var url string
	err := s.db.QueryRowContext(ctx, `SELECT url FROM emsp_endpoints WHERE connection_id=$1 AND platform='remote' AND identifier=$2 AND ($3='' OR role=$3) ORDER BY id LIMIT 1`, runID, identifier, role).Scan(&url)
	return url, err
}

func (s *Store) RemoteEndpoints(ctx context.Context, runID string) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT identifier,role,url FROM emsp_endpoints WHERE connection_id=$1 AND platform='remote' ORDER BY id`, runID)
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
	_, err = s.db.ExecContext(ctx, `INSERT INTO emsp_tokens(connection_id,country_code,party_id,uid,type,body,last_updated)
		VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(connection_id,country_code,party_id,uid,type)
		DO UPDATE SET body=excluded.body,last_updated=excluded.last_updated,updated_at=now()`, runID, text(body["country_code"]), text(body["party_id"]), text(body["uid"]), text(body["type"]), raw, updated)
	return err
}

func (s *Store) Token(ctx context.Context, runID, uid, tokenType string) (map[string]any, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT body FROM emsp_tokens WHERE connection_id=$1 AND uid=$2 AND type=$3`, runID, uid, tokenType).Scan(&raw)
	var body map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &body)
	}
	return body, err
}

func (s *Store) Tokens(ctx context.Context, runID string, dateFrom, dateTo *time.Time, offset, limit int) ([]map[string]any, int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT body, count(*) OVER() FROM emsp_tokens WHERE connection_id=$1
		AND ($2::timestamptz IS NULL OR last_updated >= $2) AND ($3::timestamptz IS NULL OR last_updated < $3)
		ORDER BY last_updated,uid OFFSET $4 LIMIT $5`, runID, dateFrom, dateTo, offset, limit)
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
	table, err := objectTable(module)
	if err != nil {
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
	if module == "cdrs" {
		_, err = s.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(connection_id,country_code,party_id,object_id,body,last_updated) VALUES($1,$2,$3,$4,$5,$6)`, table), runID, countryCode, partyID, objectID, raw, nullableTime(updated))
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(connection_id,country_code,party_id,object_id,body,last_updated) VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT(connection_id,country_code,party_id,object_id) DO UPDATE SET body=excluded.body,last_updated=excluded.last_updated,updated_at=now()`, table), runID, countryCode, partyID, objectID, raw, nullableTime(updated))
	return err
}

func (s *Store) Object(ctx context.Context, runID, module, countryCode, partyID, objectID string) (map[string]any, error) {
	table, err := objectTable(module)
	if err != nil {
		return nil, err
	}
	var raw []byte
	err = s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT body FROM %s WHERE connection_id=$1 AND country_code=$2 AND party_id=$3 AND object_id=$4`, table), runID, countryCode, partyID, objectID).Scan(&raw)
	var body map[string]any
	if err == nil {
		err = json.Unmarshal(raw, &body)
	}
	return body, err
}

func (s *Store) DeleteObject(ctx context.Context, runID, module, countryCode, partyID, objectID string) error {
	table, err := objectTable(module)
	if err != nil {
		return err
	}
	if module != "tariffs" {
		return fmt.Errorf("delete unsupported for %s", module)
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE connection_id=$1 AND country_code=$2 AND party_id=$3 AND object_id=$4`, table), runID, countryCode, partyID, objectID)
	return err
}

func (s *Store) CreateCommand(ctx context.Context, id, runID, commandType, responseURL string, request any) error {
	raw, _ := json.Marshal(request)
	_, err := s.db.ExecContext(ctx, `INSERT INTO emsp_commands(id,connection_id,type,status,request,response_url) VALUES($1,$2,$3,'SENT',$4,$5)`, id, runID, commandType, raw, responseURL)
	return err
}

func (s *Store) UpdateCommandDirect(ctx context.Context, id, status string, timeout int, response any) error {
	raw, _ := json.Marshal(response)
	_, err := s.db.ExecContext(ctx, `UPDATE emsp_commands SET status=$2,direct_response=$3,timeout_seconds=$4,updated_at=now() WHERE id=$1`, id, status, raw, timeout)
	return err
}

func (s *Store) UpdateCommandAsync(ctx context.Context, id, status string, result any) error {
	raw, _ := json.Marshal(result)
	_, err := s.db.ExecContext(ctx, `UPDATE emsp_commands SET status=$2,async_result=$3,updated_at=now() WHERE id=$1`, id, status, raw)
	return err
}

func (s *Store) AddExchange(ctx context.Context, exchange Exchange) error {
	reqHeaders, _ := json.Marshal(exchange.RequestHeaders)
	reqBody, _ := json.Marshal(exchange.RequestBody)
	resHeaders, _ := json.Marshal(exchange.ResponseHeaders)
	resBody, _ := json.Marshal(exchange.ResponseBody)
	findings, _ := json.Marshal(exchange.Findings)
	_, err := s.db.ExecContext(ctx, `INSERT INTO emsp_exchanges(connection_id,direction,module,method,url,http_status,request_headers,request_body,response_headers,response_body,ocpi_status_code,findings,duration_ms)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, exchange.RunID, exchange.Direction, exchange.Module, exchange.Method, exchange.URL, exchange.HTTPStatus, reqHeaders, nullableJSON(reqBody), resHeaders, nullableJSON(resBody), exchange.OCPIStatusCode, findings, exchange.Duration.Milliseconds())
	return err
}

func (s *Store) State(ctx context.Context, runID string, revealSecrets bool) (map[string]json.RawMessage, error) {
	queries := map[string]string{
		"setup":     `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT id,name,status,config,created_at,updated_at FROM emsp_connections WHERE id=$1) x`,
		"endpoints": `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT platform,identifier,role,url,created_at FROM emsp_endpoints WHERE connection_id=$1 ORDER BY id) x`,
		"tokens":    `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT country_code,party_id,uid,type,body,last_updated,updated_at FROM emsp_tokens WHERE connection_id=$1 ORDER BY updated_at DESC) x`,
		"locations": `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT country_code,party_id,object_id,body,last_updated,updated_at FROM emsp_locations WHERE connection_id=$1 ORDER BY updated_at DESC) x`,
		"tariffs":   `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT country_code,party_id,object_id,body,last_updated,updated_at FROM emsp_tariffs WHERE connection_id=$1 ORDER BY updated_at DESC) x`,
		"sessions":  `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT country_code,party_id,object_id,body,last_updated,updated_at FROM emsp_sessions WHERE connection_id=$1 ORDER BY updated_at DESC) x`,
		"cdrs":      `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT country_code,party_id,object_id,body,last_updated,created_at FROM emsp_cdrs WHERE connection_id=$1 ORDER BY created_at DESC) x`,
		"commands":  `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT * FROM emsp_commands WHERE connection_id=$1 ORDER BY created_at DESC LIMIT 100) x`,
		"exchanges": `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT * FROM emsp_exchanges WHERE connection_id=$1 ORDER BY created_at DESC LIMIT 200) x`,
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
func (s *Store) partnerSecretTable(ctx context.Context, runID string, reveal bool) (json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,ciphertext,updated_at::text FROM emsp_secrets WHERE connection_id=$1 ORDER BY key`, runID)
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

func objectTable(module string) (string, error) {
	switch module {
	case "locations":
		return "emsp_locations", nil
	case "tariffs":
		return "emsp_tariffs", nil
	case "sessions":
		return "emsp_sessions", nil
	case "cdrs":
		return "emsp_cdrs", nil
	default:
		return "", fmt.Errorf("unsupported object module %q", module)
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
