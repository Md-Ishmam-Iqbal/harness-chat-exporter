package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Md-Ishmam-Iqbal/harness-chat-exporter/internal/domain"
)

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type sqliteSchema struct {
	sessionTable string
	messageTable string
	partTable    string
	sessionCols  []string
	messageCols  []string
	partCols     []string
	sessionID    string
	messageID    string
	messageSID   string
	partID       string
	partMID      string
	partSID      string
}

func discoverSQLite(ctx context.Context, root domain.DetectedRoot, emit func(domain.SessionReference) error) error {
	if !hasSQLiteHeader(root.Canonical) {
		return errors.New("OpenCode database has an invalid SQLite header")
	}
	database, err := openReadOnly(root.Canonical)
	if err != nil {
		return err
	}
	defer database.Close()
	tx, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	schema, err := inspectSchema(ctx, tx)
	if err != nil {
		return err
	}
	if schema.sessionTable == "" || schema.sessionID == "" {
		return errors.New("OpenCode database does not contain a supported session table")
	}

	query := "SELECT * FROM " + quoteIdentifier(schema.sessionTable)
	order := orderTerms(schema.sessionCols, "time_created", "time_updated", schema.sessionID)
	if order != "" {
		query += " ORDER BY " + order
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	info, _ := os.Stat(root.Canonical)
	var size int64
	var modified *time.Time
	if info != nil {
		size = info.Size()
		value := info.ModTime().UTC()
		modified = &value
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		row, err := scanCurrentRow(rows)
		if err != nil {
			return err
		}
		id := valueString(row[schema.sessionID])
		if id == "" {
			continue
		}
		started := firstTime(row, "time_created", "created_at", "created")
		updated := firstTime(row, "time_updated", "updated_at", "updated")
		updatedSource := "session"
		if updated == nil && modified != nil {
			value := *modified
			updated = &value
			updatedSource = "filesystem"
		}
		metadata := map[string]string{
			"database_path": root.Canonical, "session_table": schema.sessionTable,
			"message_table": schema.messageTable, "part_table": schema.partTable,
			"layout": sourceSQLite, "updated_at_source": updatedSource,
		}
		copyStringMetadata(metadata, row, "parent_id", "directory", "title", "version", "project_id", "workspace_id", "time_archived")
		normalizeMetadata(metadata, row)
		if err := emit(domain.SessionReference{
			HarnessID: "opencode", CanonicalSourceRoot: root.Canonical,
			CanonicalSourceIdentity: root.Canonical + "#session=" + id,
			DisplayPath:             root.Path + "#session=" + id, NativeSessionID: id,
			SizeBytes: size, SourceModifiedAt: modified, CandidateStartedAt: started, CandidateUpdatedAt: updated,
			SourceKind: sourceSQLite, SourceVersion: sqliteVersion(schema),
			Metadata: metadata,
		}); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

func probeSQLite(ctx context.Context, reference domain.SessionReference) domain.ProbeResult {
	path := reference.Metadata["database_path"]
	if path == "" || !hasSQLiteHeader(path) {
		return diagnostic("unsupported_format", errors.New("invalid OpenCode SQLite source"))
	}
	database, err := openReadOnly(path)
	if err != nil {
		return diagnostic("read_error", err)
	}
	defer database.Close()
	tx, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return diagnostic("read_error", err)
	}
	defer tx.Rollback()
	schema, err := inspectSchema(ctx, tx)
	if err != nil || schema.sessionTable == "" || schema.sessionID == "" {
		if err == nil {
			err = errors.New("unsupported OpenCode SQLite schema")
		}
		return diagnostic("unsupported_format", err)
	}
	row, err := selectOneMap(ctx, tx, schema.sessionTable, schema.sessionID, reference.NativeSessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return diagnostic("read_error", errors.New("OpenCode session row no longer exists"))
		}
		return diagnostic("read_error", err)
	}
	started := firstTime(row, "time_created", "created_at", "created")
	updated := firstTime(row, "time_updated", "updated_at", "updated")
	warnings := schemaWarnings(schema)
	return domain.ProbeResult{Supported: true, NativeSessionID: valueString(row[schema.sessionID]),
		SourceKind: sourceSQLite, SourceVersion: sqliteVersion(schema), StartedAt: started,
		UpdatedAt: updated, Warnings: warnings}
}

func parseSQLite(ctx context.Context, reference domain.SessionReference, sink domain.NativeEventSink) domain.ParseResult {
	path := reference.Metadata["database_path"]
	if path == "" || !hasSQLiteHeader(path) {
		return failedParse("unsupported_format", errors.New("invalid OpenCode SQLite source"))
	}
	database, err := openReadOnly(path)
	if err != nil {
		return failedParse("read_error", err)
	}
	defer database.Close()
	tx, err := database.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return failedParse("read_error", err)
	}
	defer tx.Rollback()
	schema, err := inspectSchema(ctx, tx)
	if err != nil {
		return failedParse("unsupported_format", err)
	}
	result := domain.ParseResult{Warnings: schemaWarnings(schema)}
	if len(result.Warnings) > 0 {
		result.Partial = true
	}
	sessionRow, err := selectOneMap(ctx, tx, schema.sessionTable, schema.sessionID, reference.NativeSessionID)
	if err != nil {
		return failedParse("read_error", err)
	}
	sessionNative := marshalNative(map[string]any{"session": sessionRow, "storage_generation": sourceSQLite})
	sessionEvent := metadataEvent("session:"+reference.NativeSessionID, "sqlite.session", sessionNative,
		firstTime(sessionRow, "time_created", "created_at", "created"), 0)
	if err := sink(ctx, sessionEvent); err != nil {
		return failedParse("normalization_error", err)
	}

	if schema.messageTable == "" || schema.partTable == "" || schema.messageSID == "" || schema.partMID == "" {
		result.Partial = true
		result.Warnings = append(result.Warnings, warn("opencode_incomplete_sqlite_schema", "unsupported_format", "message/part tables or relationship columns are missing"))
		return result
	}

	query := joinedQuery(schema)
	rows, err := tx.QueryContext(ctx, query, reference.NativeSessionID)
	if err != nil {
		return failedParse("read_error", err)
	}
	defer rows.Close()
	var lastMessageID string
	var partCountForMessage int64
	var index int64 = 1
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return failedParse("cancelled", err)
		}
		messageRow, partRow, err := scanJoinedRow(rows, len(schema.messageCols), len(schema.partCols))
		if err != nil {
			return failedParse("read_error", err)
		}
		messageID := valueString(messageRow[schema.messageID])
		if messageID != lastMessageID {
			if lastMessageID != "" && partCountForMessage == 0 {
				result.Partial = true
				result.Warnings = append(result.Warnings, warn("opencode_message_without_parts", "integrity_warning", "message has no related part rows"))
			}
			lastMessageID = messageID
			partCountForMessage = 0
			native := marshalNative(map[string]any{"message": messageRow, "storage_generation": sourceSQLite})
			event := mapMessageMetadata(messageRow, messageID, native, index)
			if err := sink(ctx, event); err != nil {
				return failedParse("normalization_error", err)
			}
			index++
		}
		if !rowPresent(partRow, schema.partID) {
			continue
		}
		partCountForMessage++
		events, mapWarnings := mapPart(messageRow, partRow, sessionRow, index, sourceSQLite)
		if len(mapWarnings) > 0 {
			result.Warnings = append(result.Warnings, mapWarnings...)
			result.Partial = true
		}
		for _, event := range events {
			if err := sink(ctx, event); err != nil {
				return failedParse("normalization_error", err)
			}
			index++
		}
	}
	if err := rows.Err(); err != nil {
		return failedParse("read_error", err)
	}
	if lastMessageID != "" && partCountForMessage == 0 {
		result.Partial = true
		result.Warnings = append(result.Warnings, warn("opencode_message_without_parts", "integrity_warning", "message has no related part rows"))
	}
	if err := tx.Commit(); err != nil {
		return failedParse("read_error", err)
	}
	return result
}

func inspectSchema(ctx context.Context, q queryer) (sqliteSchema, error) {
	tables := map[string]string{}
	rows, err := q.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if err != nil {
		return sqliteSchema{}, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return sqliteSchema{}, err
		}
		tables[strings.ToLower(name)] = name
	}
	if err := rows.Close(); err != nil {
		return sqliteSchema{}, err
	}
	schema := sqliteSchema{
		sessionTable: firstTable(tables, "session", "sessions"),
		messageTable: firstTable(tables, "message", "messages"),
		partTable:    firstTable(tables, "part", "parts"),
	}
	for table, destination := range map[string]*[]string{
		schema.sessionTable: &schema.sessionCols,
		schema.messageTable: &schema.messageCols,
		schema.partTable:    &schema.partCols,
	} {
		if table == "" {
			continue
		}
		columns, err := tableColumns(ctx, q, table)
		if err != nil {
			return sqliteSchema{}, err
		}
		*destination = columns
	}
	schema.sessionID = firstColumn(schema.sessionCols, "id", "session_id", "sessionID")
	schema.messageID = firstColumn(schema.messageCols, "id", "message_id", "messageID")
	schema.messageSID = firstColumn(schema.messageCols, "session_id", "sessionID")
	schema.partID = firstColumn(schema.partCols, "id", "part_id", "partID")
	schema.partMID = firstColumn(schema.partCols, "message_id", "messageID")
	schema.partSID = firstColumn(schema.partCols, "session_id", "sessionID")
	return schema, nil
}

func tableColumns(ctx context.Context, q queryer, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA table_info("+quoteIdentifier(table)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var cid int64
		var name, dataType string
		var notNull, primaryKey int64
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		result = append(result, name)
	}
	return result, rows.Err()
}

func selectOneMap(ctx context.Context, q queryer, table, idColumn, id string) (map[string]any, error) {
	rows, err := q.QueryContext(ctx, "SELECT * FROM "+quoteIdentifier(table)+" WHERE "+quoteIdentifier(idColumn)+" = ?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, sql.ErrNoRows
	}
	return scanCurrentRow(rows)
}

func scanCurrentRow(rows *sql.Rows) (map[string]any, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	values := make([]any, len(columns))
	targets := make([]any, len(columns))
	for index := range values {
		targets[index] = &values[index]
	}
	if err := rows.Scan(targets...); err != nil {
		return nil, err
	}
	result := make(map[string]any, len(columns))
	for index, name := range columns {
		result[name] = normalizeSQLValue(name, values[index])
	}
	return result, nil
}

func scanJoinedRow(rows *sql.Rows, messageColumns, partColumns int) (map[string]any, map[string]any, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	values := make([]any, len(columns))
	targets := make([]any, len(columns))
	for index := range values {
		targets[index] = &values[index]
	}
	if err := rows.Scan(targets...); err != nil {
		return nil, nil, err
	}
	message := make(map[string]any, messageColumns)
	part := make(map[string]any, partColumns)
	for index, alias := range columns {
		name := strings.TrimPrefix(strings.TrimPrefix(alias, "m__"), "p__")
		value := normalizeSQLValue(name, values[index])
		if strings.HasPrefix(alias, "m__") {
			message[name] = value
		} else {
			part[name] = value
		}
	}
	return message, part, nil
}

func normalizeSQLValue(column string, value any) any {
	if value == nil {
		return nil
	}
	if strings.EqualFold(column, "data") {
		switch item := value.(type) {
		case string:
			if json.Valid([]byte(item)) {
				return json.RawMessage(item)
			}
		case []byte:
			if json.Valid(item) {
				return json.RawMessage(append([]byte(nil), item...))
			}
		}
	}
	if bytes, ok := value.([]byte); ok {
		return append([]byte(nil), bytes...)
	}
	return value
}

func joinedQuery(schema sqliteSchema) string {
	selects := make([]string, 0, len(schema.messageCols)+len(schema.partCols))
	for _, column := range schema.messageCols {
		selects = append(selects, "m."+quoteIdentifier(column)+" AS "+quoteIdentifier("m__"+column))
	}
	for _, column := range schema.partCols {
		selects = append(selects, "p."+quoteIdentifier(column)+" AS "+quoteIdentifier("p__"+column))
	}
	query := "SELECT " + strings.Join(selects, ",") + " FROM " + quoteIdentifier(schema.messageTable) + " m LEFT JOIN " +
		quoteIdentifier(schema.partTable) + " p ON p." + quoteIdentifier(schema.partMID) + " = m." + quoteIdentifier(schema.messageID) +
		" WHERE m." + quoteIdentifier(schema.messageSID) + " = ?"
	orders := prefixedOrderTerms("m", schema.messageCols, "time_created", schema.messageID)
	orders = append(orders, prefixedOrderTerms("p", schema.partCols, "time_created", schema.partID)...)
	if len(orders) > 0 {
		query += " ORDER BY " + strings.Join(orders, ",")
	}
	return query
}

func prefixedOrderTerms(prefix string, columns []string, candidates ...string) []string {
	var result []string
	for _, candidate := range candidates {
		if column := firstColumn(columns, candidate); column != "" {
			result = append(result, prefix+"."+quoteIdentifier(column))
		}
	}
	return result
}

func orderTerms(columns []string, candidates ...string) string {
	var result []string
	for _, candidate := range candidates {
		if column := firstColumn(columns, candidate); column != "" {
			result = append(result, quoteIdentifier(column))
		}
	}
	return strings.Join(result, ",")
}

func firstTable(tables map[string]string, candidates ...string) string {
	for _, candidate := range candidates {
		if value := tables[strings.ToLower(candidate)]; value != "" {
			return value
		}
	}
	return ""
}

func firstColumn(columns []string, candidates ...string) string {
	for _, candidate := range candidates {
		for _, column := range columns {
			if strings.EqualFold(column, candidate) {
				return column
			}
		}
	}
	return ""
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

func sqliteVersion(schema sqliteSchema) string {
	if schema.messageTable == "" || schema.partTable == "" {
		return "sqlite-partial"
	}
	return "sqlite-session-message-part"
}

func schemaWarnings(schema sqliteSchema) []domain.Warning {
	var result []domain.Warning
	if schema.sessionTable == "" || schema.sessionID == "" {
		result = append(result, warn("opencode_missing_session_table", "unsupported_format", "session table or identifier column is missing"))
	}
	if schema.messageTable == "" || schema.messageID == "" || schema.messageSID == "" {
		result = append(result, warn("opencode_missing_message_table", "unsupported_format", "message table or relationship columns are missing"))
	}
	if schema.partTable == "" || schema.partID == "" || schema.partMID == "" {
		result = append(result, warn("opencode_missing_part_table", "unsupported_format", "part table or relationship columns are missing"))
	}
	return result
}

func copyStringMetadata(destination map[string]string, row map[string]any, keys ...string) {
	for _, key := range keys {
		if value := valueString(row[key]); value != "" {
			destination[key] = value
		}
	}
}

func valueString(value any) string {
	switch item := value.(type) {
	case string:
		return item
	case []byte:
		return string(item)
	case json.RawMessage:
		var result string
		if json.Unmarshal(item, &result) == nil {
			return result
		}
	}
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func firstTime(row map[string]any, keys ...string) *time.Time {
	for _, key := range keys {
		if value := parseTime(row[key]); value != nil {
			return value
		}
	}
	return nil
}

func rowPresent(row map[string]any, idColumn string) bool {
	return idColumn != "" && row[idColumn] != nil && valueString(row[idColumn]) != ""
}

func marshalNative(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}
