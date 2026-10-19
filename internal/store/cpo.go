package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type CPOObject struct {
	Module      string
	ObjectID    string
	Body        json.RawMessage
	Snapshot    json.RawMessage
	Delivery    string
	Findings    json.RawMessage
	CDRLocation string
	UpdatedAt   string
}

type CPOCommand struct {
	ID          string
	Type        string
	Status      string
	Request     json.RawMessage
	ResponseURL string
	Result      json.RawMessage
	CreatedAt   string
	UpdatedAt   string
}

type CPOSnapshot struct {
	RunID       string
	Profile     string
	Status      string
	Config      json.RawMessage
	Credentials json.RawMessage
	Secrets     map[string]string
	Endpoints   []json.RawMessage
	Objects     []CPOObject
	Commands    []CPOCommand
	Exchanges   []json.RawMessage
}

// ocpiTime renders a timestamp in the OCPI layout the CPO controller writes, so restored times
// sort and parse like fresh ones.
const ocpiTime = `'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'`

// cpoExchangeJSON rebuilds an exchange exactly as the CPO controller marshals it.
const cpoExchangeJSON = `jsonb_build_object('id',exchange_id,'direction',direction,'module',module,'method',method,'url',url,'http_status',http_status,
	'request_headers',request_headers,'response_headers',response_headers,'findings',findings,'duration_ms',duration_ms,
	'created_at',to_char(created_at AT TIME ZONE 'UTC',` + ocpiTime + `))
	|| CASE WHEN COALESCE(ocpi_status_code,0)<>0 THEN jsonb_build_object('ocpi_status_code',ocpi_status_code) ELSE '{}'::jsonb END
	|| CASE WHEN request_body IS NOT NULL THEN jsonb_build_object('request_body',request_body) ELSE '{}'::jsonb END
	|| CASE WHEN response_body IS NOT NULL THEN jsonb_build_object('response_body',response_body) ELSE '{}'::jsonb END`

func (s *Store) LoadCPO(ctx context.Context) (CPOSnapshot, error) {
	return s.LoadCPOProfile(ctx, "cpo")
}

func (s *Store) LoadCPOProfile(ctx context.Context, profile string) (CPOSnapshot, error) {
	var snapshot CPOSnapshot
	snapshot.Profile = profile
	var secrets, endpoints []byte
	if err := s.db.QueryRowContext(ctx, `SELECT id,status,config,credentials,secrets,endpoints FROM partner_console_connections WHERE leg='cpo' AND profile=$1 AND env=$2 ORDER BY updated_at DESC LIMIT 1`, profile, s.env).Scan(&snapshot.RunID, &snapshot.Status, &snapshot.Config, &snapshot.Credentials, &secrets, &endpoints); err != nil {
		return snapshot, err
	}
	var sealed map[string]struct {
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal(secrets, &sealed); err != nil {
		return snapshot, err
	}
	snapshot.Secrets = make(map[string]string, len(sealed))
	for key, item := range sealed {
		value, err := s.decryptCPOSecret(snapshot.RunID, key, item.Ciphertext)
		if err != nil {
			return snapshot, err
		}
		snapshot.Secrets[key] = value
	}
	if err := json.Unmarshal(endpoints, &snapshot.Endpoints); err != nil {
		return snapshot, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT module,object_id,body,snapshot,delivery,findings,cdr_location,to_char(updated_at AT TIME ZONE 'UTC',`+ocpiTime+`) FROM partner_console_objects WHERE connection_id=$1 ORDER BY module,object_id`, snapshot.RunID)
	if err != nil {
		return snapshot, err
	}
	for rows.Next() {
		var item CPOObject
		if err := rows.Scan(&item.Module, &item.ObjectID, &item.Body, &item.Snapshot, &item.Delivery, &item.Findings, &item.CDRLocation, &item.UpdatedAt); err != nil {
			rows.Close()
			return snapshot, err
		}
		snapshot.Objects = append(snapshot.Objects, item)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT id,type,status,request,response_url,COALESCE(result,'null'::jsonb),to_char(created_at AT TIME ZONE 'UTC',`+ocpiTime+`),to_char(updated_at AT TIME ZONE 'UTC',`+ocpiTime+`) FROM partner_console_commands WHERE connection_id=$1 ORDER BY created_at`, snapshot.RunID)
	if err != nil {
		return snapshot, err
	}
	for rows.Next() {
		var item CPOCommand
		if err := rows.Scan(&item.ID, &item.Type, &item.Status, &item.Request, &item.ResponseURL, &item.Result, &item.CreatedAt, &item.UpdatedAt); err != nil {
			rows.Close()
			return snapshot, err
		}
		snapshot.Commands = append(snapshot.Commands, item)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT `+cpoExchangeJSON+` FROM partner_console_exchanges WHERE connection_id=$1 ORDER BY created_at DESC LIMIT 300`, snapshot.RunID)
	if err != nil {
		return snapshot, err
	}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return snapshot, err
		}
		snapshot.Exchanges = append(snapshot.Exchanges, raw)
	}
	rows.Close()
	return snapshot, rows.Err()
}

func (s *Store) SaveCPO(ctx context.Context, snapshot CPOSnapshot) error {
	if snapshot.Profile == "" {
		snapshot.Profile = "cpo"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	sealed := make(map[string]string, len(snapshot.Secrets))
	for key, value := range snapshot.Secrets {
		if value == "" {
			continue
		}
		ciphertext, err := s.encryptCPOSecret(snapshot.RunID, key, value)
		if err != nil {
			return err
		}
		sealed[key] = ciphertext
	}
	secrets, _ := json.Marshal(sealed)
	endpointList := make([]map[string]string, 0, len(snapshot.Endpoints))
	for _, raw := range snapshot.Endpoints {
		var endpoint struct{ Identifier, Role, URL string }
		if err := json.Unmarshal(raw, &endpoint); err != nil {
			return err
		}
		endpointList = append(endpointList, map[string]string{"identifier": endpoint.Identifier, "role": endpoint.Role, "url": endpoint.URL})
	}
	endpoints, _ := json.Marshal(endpointList)
	if _, err := tx.ExecContext(ctx, `INSERT INTO partner_console_connections(id,leg,profile,status,config,credentials,secrets,endpoints,env)
		VALUES($1,'cpo',$2,$3,$4,$5,(SELECT COALESCE(jsonb_object_agg(k,jsonb_build_object('ciphertext',v,'updated_at',now()::text)),'{}'::jsonb) FROM jsonb_each_text($6::jsonb) AS t(k,v)),$7,$8)
		ON CONFLICT(id) DO UPDATE SET status=excluded.status,config=excluded.config,credentials=excluded.credentials,secrets=excluded.secrets,endpoints=excluded.endpoints,updated_at=now()`,
		snapshot.RunID, snapshot.Profile, snapshot.Status, snapshot.Config, snapshot.Credentials, secrets, endpoints, s.env); err != nil {
		return err
	}
	// Rows are upserted only when a value changed and removed when the snapshot no longer has them,
	// so a save rewrites what moved instead of the whole run.
	objectRows := make([][]any, 0, len(snapshot.Objects))
	objectKeys := make([][2]string, 0, len(snapshot.Objects))
	for _, item := range snapshot.Objects {
		objectRows = append(objectRows, []any{snapshot.RunID, item.Module, item.ObjectID, item.Body, item.Snapshot, item.Delivery, item.Findings, item.CDRLocation, item.UpdatedAt})
		objectKeys = append(objectKeys, [2]string{item.Module, item.ObjectID})
	}
	if err := insertRows(ctx, tx, `INSERT INTO partner_console_objects(connection_id,leg,module,object_id,body,snapshot,delivery,findings,cdr_location,updated_at) VALUES `, "'cpo'", objectRows,
		` ON CONFLICT(connection_id,module,country_code,party_id,object_id,token_type) DO UPDATE SET body=excluded.body,snapshot=excluded.snapshot,delivery=excluded.delivery,findings=excluded.findings,cdr_location=excluded.cdr_location,updated_at=excluded.updated_at
		WHERE (partner_console_objects.body,partner_console_objects.snapshot,partner_console_objects.delivery,partner_console_objects.findings,partner_console_objects.cdr_location,partner_console_objects.updated_at) IS DISTINCT FROM (excluded.body,excluded.snapshot,excluded.delivery,excluded.findings,excluded.cdr_location,excluded.updated_at)`); err != nil {
		return err
	}
	keys, _ := json.Marshal(objectKeys)
	if _, err := tx.ExecContext(ctx, `DELETE FROM partner_console_objects WHERE connection_id=$1 AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements($2::jsonb) AS k WHERE k->>0=partner_console_objects.module AND k->>1=partner_console_objects.object_id)`, snapshot.RunID, keys); err != nil {
		return err
	}

	commandRows := make([][]any, 0, len(snapshot.Commands))
	commandIDs := make([]string, 0, len(snapshot.Commands))
	for _, item := range snapshot.Commands {
		commandRows = append(commandRows, []any{snapshot.RunID, item.ID, item.Type, item.Status, item.Request, item.ResponseURL, nullableJSON(item.Result), item.CreatedAt, item.UpdatedAt})
		commandIDs = append(commandIDs, item.ID)
	}
	if err := insertRows(ctx, tx, `INSERT INTO partner_console_commands(connection_id,leg,id,type,status,request,response_url,result,created_at,updated_at) VALUES `, "'cpo'", commandRows,
		` ON CONFLICT(connection_id,id) DO UPDATE SET type=excluded.type,status=excluded.status,request=excluded.request,response_url=excluded.response_url,result=excluded.result,created_at=excluded.created_at,updated_at=excluded.updated_at
		WHERE (partner_console_commands.type,partner_console_commands.status,partner_console_commands.request,partner_console_commands.response_url,partner_console_commands.result,partner_console_commands.created_at,partner_console_commands.updated_at) IS DISTINCT FROM (excluded.type,excluded.status,excluded.request,excluded.response_url,excluded.result,excluded.created_at,excluded.updated_at)`); err != nil {
		return err
	}
	ids, _ := json.Marshal(commandIDs)
	if _, err := tx.ExecContext(ctx, `DELETE FROM partner_console_commands WHERE connection_id=$1 AND id <> ALL(ARRAY(SELECT jsonb_array_elements_text($2::jsonb)))`, snapshot.RunID, ids); err != nil {
		return err
	}

	// Exchanges never change once recorded: insert the new ones. The ones trimmed from memory stay in the
	// database as history; Reset removes them with the connection.
	exchangeRows := make([][]any, 0, len(snapshot.Exchanges))
	for _, raw := range snapshot.Exchanges {
		var exchange struct {
			ID              string          `json:"id"`
			Direction       string          `json:"direction"`
			Module          string          `json:"module"`
			Method          string          `json:"method"`
			URL             string          `json:"url"`
			HTTPStatus      int             `json:"http_status"`
			OCPIStatusCode  *int            `json:"ocpi_status_code"`
			RequestHeaders  json.RawMessage `json:"request_headers"`
			RequestBody     json.RawMessage `json:"request_body"`
			ResponseHeaders json.RawMessage `json:"response_headers"`
			ResponseBody    json.RawMessage `json:"response_body"`
			Findings        json.RawMessage `json:"findings"`
			DurationMS      int64           `json:"duration_ms"`
			CreatedAt       string          `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &exchange); err != nil {
			return err
		}
		exchangeRows = append(exchangeRows, []any{snapshot.RunID, exchange.ID, exchange.Direction, exchange.Module, exchange.Method, exchange.URL, exchange.HTTPStatus, exchange.OCPIStatusCode,
			rawOrNil(exchange.RequestHeaders), rawOrNil(exchange.RequestBody), rawOrNil(exchange.ResponseHeaders), rawOrNil(exchange.ResponseBody), rawOrNil(exchange.Findings), exchange.DurationMS, exchange.CreatedAt})
	}
	if err := insertRows(ctx, tx, `INSERT INTO partner_console_exchanges(connection_id,exchange_id,direction,module,method,url,http_status,ocpi_status_code,request_headers,request_body,response_headers,response_body,findings,duration_ms,created_at) VALUES `, "", exchangeRows,
		` ON CONFLICT(connection_id,exchange_id) WHERE exchange_id IS NOT NULL DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit()
}

// insertRows writes rows in one statement. A non-empty literal goes in as the second value of every
// row (the leg), and suffix is appended after the VALUES list (an ON CONFLICT clause).
func insertRows(ctx context.Context, tx *sql.Tx, prefix, literal string, rows [][]any, suffix string) error {
	if len(rows) == 0 {
		return nil
	}
	width := len(rows[0])
	values := make([]string, 0, len(rows))
	args := make([]any, 0, len(rows)*width)
	for rowIndex, row := range rows {
		if len(row) != width {
			return fmt.Errorf("batch row %d has %d columns, want %d", rowIndex, len(row), width)
		}
		placeholders := make([]string, 0, width+1)
		for columnIndex := range row {
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)+columnIndex+1))
			if columnIndex == 0 && literal != "" {
				placeholders = append(placeholders, literal)
			}
		}
		values = append(values, "("+strings.Join(placeholders, ",")+")")
		args = append(args, row...)
	}
	_, err := tx.ExecContext(ctx, prefix+strings.Join(values, ",")+suffix, args...)
	return err
}

func (s *Store) DeleteCPO(ctx context.Context, runID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM partner_console_connections WHERE id=$1 AND leg='cpo'`, runID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) CPOTables(ctx context.Context, runID string, revealSecrets bool) (map[string]json.RawMessage, error) {
	queries := map[string]string{
		"setup":     `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT id,status,config,created_at,updated_at FROM partner_console_connections WHERE id=$1) x`,
		"endpoints": `SELECT COALESCE((SELECT endpoints FROM partner_console_connections WHERE id=$1), '[]'::jsonb)`,
		"locations": cpoObjectQuery("locations", "object_id,body,delivery,findings,updated_at"),
		"tariffs":   cpoObjectQuery("tariffs", "object_id,body,delivery,findings,updated_at"),
		"sessions":  cpoObjectQuery("sessions", "object_id,body,delivery,findings,updated_at"),
		"cdrs":      cpoObjectQuery("cdrs", "object_id,body,delivery,cdr_location,findings,updated_at"),
		"tokens":    cpoObjectQuery("tokens", "object_id,body,delivery,findings,updated_at"),
		"commands":  `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT id,type,status,request,response_url,result,created_at,updated_at FROM partner_console_commands WHERE connection_id=$1 ORDER BY created_at DESC) x`,
		"exchanges": `SELECT COALESCE(jsonb_agg(x.entry ORDER BY x.created_at DESC), '[]'::jsonb) FROM (SELECT ` + cpoExchangeJSON + ` AS entry,created_at FROM partner_console_exchanges WHERE connection_id=$1 ORDER BY created_at DESC LIMIT 300) x`,
	}
	result := make(map[string]json.RawMessage, len(queries)+1)
	for name, query := range queries {
		var raw json.RawMessage
		if err := s.db.QueryRowContext(ctx, query, runID).Scan(&raw); err != nil {
			return nil, err
		}
		result[name] = raw
	}
	secrets, err := s.cpoLabSecretTable(ctx, runID, revealSecrets)
	if err != nil {
		return nil, err
	}
	result["secrets"] = secrets
	return result, nil
}

func cpoObjectQuery(module, columns string) string {
	return `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT ` + columns + ` FROM partner_console_objects WHERE connection_id=$1 AND module='` + module + `' ORDER BY updated_at DESC) x`
}

func (s *Store) cpoLabSecretTable(ctx context.Context, runID string, reveal bool) (json.RawMessage, error) {
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
			value, err = s.decryptCPOSecret(runID, key, ciphertext)
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

// cpoLabSecretRow is the shape one secret takes on the wire. A row that was not revealed carries
// no value, no fingerprint and no header — the tab shows only that the key exists and when it
// changed, which is what it showed before the reveal button existed.
func cpoLabSecretRow(key, updatedAt, value string, reveal bool) map[string]any {
	row := map[string]any{"key": key, "encrypted": true, "updated_at": updatedAt}
	if !reveal {
		return row
	}
	row["value"] = value
	row["fingerprint"] = secretFingerprint(value)
	// OCPI sends Authorization: Token base64(token), and building that by hand is where QA loses
	// time — the same reason the Tokens panel ships it beside the raw value.
	row["authorization"] = "Token " + base64.StdEncoding.EncodeToString([]byte(value))
	return row
}

// secretFingerprint names a secret without printing it, so a baseline taken before a rotate can be
// compared afterwards from the browser. Eight bytes of SHA-256 is far past what a collision would
// need here, and the full digest would not fit the row it is shown on.
func secretFingerprint(value string) string {
	digest := sha256.Sum256([]byte(value))
	return fmt.Sprintf("sha256:%x", digest[:8])
}

func (s *Store) encryptCPOSecret(runID, key, value string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := s.aead.Seal(nonce, nonce, []byte(value), []byte("cpo:"+runID+":"+key))
	return base64.RawStdEncoding.EncodeToString(ciphertext), nil
}

func (s *Store) decryptCPOSecret(runID, key, encoded string) (string, error) {
	ciphertext, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(ciphertext) < s.aead.NonceSize() {
		return "", fmt.Errorf("invalid encrypted CPO secret")
	}
	nonce, payload := ciphertext[:s.aead.NonceSize()], ciphertext[s.aead.NonceSize():]
	plain, err := s.aead.Open(nil, nonce, payload, []byte("cpo:"+runID+":"+key))
	return string(plain), err
}
