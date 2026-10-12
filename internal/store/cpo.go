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

func (s *Store) LoadCPO(ctx context.Context) (CPOSnapshot, error) {
	return s.LoadCPOProfile(ctx, "cpo")
}

func (s *Store) LoadCPOProfile(ctx context.Context, profile string) (CPOSnapshot, error) {
	var snapshot CPOSnapshot
	snapshot.Profile = profile
	if err := s.db.QueryRowContext(ctx, `SELECT id,status,config,credentials FROM cpo_connections WHERE profile=$1 ORDER BY updated_at DESC LIMIT 1`, profile).Scan(&snapshot.RunID, &snapshot.Status, &snapshot.Config, &snapshot.Credentials); err != nil {
		return snapshot, err
	}
	snapshot.Secrets = map[string]string{}
	rows, err := s.db.QueryContext(ctx, `SELECT key,ciphertext FROM cpo_secrets WHERE connection_id=$1`, snapshot.RunID)
	if err != nil {
		return snapshot, err
	}
	for rows.Next() {
		var key, ciphertext string
		if err := rows.Scan(&key, &ciphertext); err != nil {
			rows.Close()
			return snapshot, err
		}
		value, err := s.decryptCPOSecret(snapshot.RunID, key, ciphertext)
		if err != nil {
			rows.Close()
			return snapshot, err
		}
		snapshot.Secrets[key] = value
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT jsonb_build_object('identifier',identifier,'role',role,'url',url) FROM cpo_endpoints WHERE connection_id=$1 ORDER BY id`, snapshot.RunID)
	if err != nil {
		return snapshot, err
	}
	for rows.Next() {
		var raw json.RawMessage
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return snapshot, err
		}
		snapshot.Endpoints = append(snapshot.Endpoints, raw)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `SELECT module,object_id,body,snapshot,delivery,findings,cdr_location,updated_at::text FROM cpo_objects WHERE connection_id=$1 ORDER BY module,object_id`, snapshot.RunID)
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

	rows, err = s.db.QueryContext(ctx, `SELECT id,type,status,request,response_url,COALESCE(result,'null'::jsonb),created_at::text,updated_at::text FROM cpo_commands WHERE connection_id=$1 ORDER BY created_at`, snapshot.RunID)
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

	rows, err = s.db.QueryContext(ctx, `SELECT payload FROM cpo_exchanges WHERE connection_id=$1 ORDER BY created_at DESC LIMIT 300`, snapshot.RunID)
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO cpo_connections(id,status,config,credentials,profile) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(id) DO UPDATE SET status=excluded.status,config=excluded.config,credentials=excluded.credentials,updated_at=now()`, snapshot.RunID, snapshot.Status, snapshot.Config, snapshot.Credentials, snapshot.Profile); err != nil {
		return err
	}
	for _, table := range []string{"cpo_secrets", "cpo_endpoints", "cpo_objects", "cpo_commands", "cpo_exchanges"} {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE connection_id=$1`, table), snapshot.RunID); err != nil {
			return err
		}
	}
	secretRows := make([][]any, 0, len(snapshot.Secrets))
	for key, value := range snapshot.Secrets {
		if value == "" {
			continue
		}
		ciphertext, err := s.encryptCPOSecret(snapshot.RunID, key, value)
		if err != nil {
			return err
		}
		secretRows = append(secretRows, []any{snapshot.RunID, key, ciphertext})
	}
	if err := insertRows(ctx, tx, `INSERT INTO cpo_secrets(connection_id,key,ciphertext) VALUES `, secretRows); err != nil {
		return err
	}
	endpointRows := make([][]any, 0, len(snapshot.Endpoints))
	for _, raw := range snapshot.Endpoints {
		var endpoint struct{ Identifier, Role, URL string }
		if err := json.Unmarshal(raw, &endpoint); err != nil {
			return err
		}
		endpointRows = append(endpointRows, []any{snapshot.RunID, endpoint.Identifier, endpoint.Role, endpoint.URL})
	}
	if err := insertRows(ctx, tx, `INSERT INTO cpo_endpoints(connection_id,identifier,role,url) VALUES `, endpointRows); err != nil {
		return err
	}
	objectRows := make([][]any, 0, len(snapshot.Objects))
	for _, item := range snapshot.Objects {
		objectRows = append(objectRows, []any{snapshot.RunID, item.Module, item.ObjectID, item.Body, item.Snapshot, item.Delivery, item.Findings, item.CDRLocation, item.UpdatedAt})
	}
	if err := insertRows(ctx, tx, `INSERT INTO cpo_objects(connection_id,module,object_id,body,snapshot,delivery,findings,cdr_location,updated_at) VALUES `, objectRows); err != nil {
		return err
	}
	commandRows := make([][]any, 0, len(snapshot.Commands))
	for _, item := range snapshot.Commands {
		commandRows = append(commandRows, []any{snapshot.RunID, item.ID, item.Type, item.Status, item.Request, item.ResponseURL, nullableJSON(item.Result), item.CreatedAt, item.UpdatedAt})
	}
	if err := insertRows(ctx, tx, `INSERT INTO cpo_commands(connection_id,id,type,status,request,response_url,result,created_at,updated_at) VALUES `, commandRows); err != nil {
		return err
	}
	exchangeRows := make([][]any, 0, len(snapshot.Exchanges))
	for _, raw := range snapshot.Exchanges {
		var meta struct {
			ID        string `json:"id"`
			CreatedAt string `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			return err
		}
		exchangeRows = append(exchangeRows, []any{snapshot.RunID, meta.ID, raw, meta.CreatedAt})
	}
	if err := insertRows(ctx, tx, `INSERT INTO cpo_exchanges(connection_id,id,payload,created_at) VALUES `, exchangeRows); err != nil {
		return err
	}
	return tx.Commit()
}

func insertRows(ctx context.Context, tx *sql.Tx, prefix string, rows [][]any) error {
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
		placeholders := make([]string, width)
		for columnIndex := range row {
			placeholders[columnIndex] = fmt.Sprintf("$%d", len(args)+columnIndex+1)
		}
		values = append(values, "("+strings.Join(placeholders, ",")+")")
		args = append(args, row...)
	}
	_, err := tx.ExecContext(ctx, prefix+strings.Join(values, ","), args...)
	return err
}

func (s *Store) DeleteCPO(ctx context.Context, runID string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM cpo_connections WHERE id=$1`, runID)
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
		"setup":     `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT id,status,config,created_at,updated_at FROM cpo_connections WHERE id=$1) x`,
		"endpoints": `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT identifier,role,url FROM cpo_endpoints WHERE connection_id=$1 ORDER BY id) x`,
		"locations": `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT object_id,body,delivery,findings,updated_at FROM cpo_objects WHERE connection_id=$1 AND module='locations' ORDER BY updated_at DESC) x`,
		"tariffs":   `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT object_id,body,delivery,findings,updated_at FROM cpo_objects WHERE connection_id=$1 AND module='tariffs' ORDER BY updated_at DESC) x`,
		"sessions":  `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT object_id,body,delivery,findings,updated_at FROM cpo_objects WHERE connection_id=$1 AND module='sessions' ORDER BY updated_at DESC) x`,
		"cdrs":      `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT object_id,body,delivery,cdr_location,findings,updated_at FROM cpo_objects WHERE connection_id=$1 AND module='cdrs' ORDER BY updated_at DESC) x`,
		"tokens":    `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT object_id,body,delivery,findings,updated_at FROM cpo_objects WHERE connection_id=$1 AND module='tokens' ORDER BY updated_at DESC) x`,
		"commands":  `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT id,type,status,request,response_url,result,created_at,updated_at FROM cpo_commands WHERE connection_id=$1 ORDER BY created_at DESC) x`,
		"exchanges": `SELECT COALESCE(jsonb_agg(payload ORDER BY created_at DESC), '[]'::jsonb) FROM cpo_exchanges WHERE connection_id=$1`,
		"favorites": `SELECT COALESCE(jsonb_agg(to_jsonb(x)), '[]'::jsonb) FROM (SELECT record_id,label,token,location_id,evse_uid,connector_id,created_at FROM cpo_favorite_tokens WHERE $1::text IS NOT NULL ORDER BY record_id) x`,
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

func (s *Store) cpoLabSecretTable(ctx context.Context, runID string, reveal bool) (json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,ciphertext,updated_at::text FROM cpo_secrets WHERE connection_id=$1 ORDER BY key`, runID)
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

type FavoriteToken struct {
	ID          string
	Label       string
	Token       map[string]any
	LocationID  string
	EVSEUID     string
	ConnectorID string
	CreatedAt   string
}

func (s *Store) FavoriteTokens(ctx context.Context) ([]FavoriteToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT record_id,label,token,location_id,evse_uid,connector_id,created_at::text FROM cpo_favorite_tokens ORDER BY record_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []FavoriteToken
	for rows.Next() {
		var item FavoriteToken
		var token []byte
		if err := rows.Scan(&item.ID, &item.Label, &token, &item.LocationID, &item.EVSEUID, &item.ConnectorID, &item.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(token, &item.Token); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) SaveFavoriteToken(ctx context.Context, favorite FavoriteToken) error {
	token, err := json.Marshal(favorite.Token)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO cpo_favorite_tokens(record_id,label,token,location_id,evse_uid,connector_id) VALUES($1,$2,$3,$4,$5,$6)
    ON CONFLICT(record_id) DO UPDATE SET label=excluded.label,token=excluded.token,location_id=excluded.location_id,evse_uid=excluded.evse_uid,connector_id=excluded.connector_id`,
		favorite.ID, favorite.Label, token, favorite.LocationID, favorite.EVSEUID, favorite.ConnectorID)
	return err
}

func (s *Store) DeleteFavoriteToken(ctx context.Context, recordID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM cpo_favorite_tokens WHERE record_id=$1`, recordID)
	return err
}
