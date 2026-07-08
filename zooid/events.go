package zooid

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
	"fiatjaf.com/nostr/khatru"
	"github.com/Masterminds/squirrel"
	"github.com/mattn/go-sqlite3"
)

type EventStore struct {
	Relay        *khatru.Relay
	Config       *Config
	Schema       *Schema
	FTSAvailable bool
}

var _ eventstore.Store = (*EventStore)(nil)

func (events *EventStore) Init() error {
	// Create basic schema first
	basicSchema := events.Schema.Render(`
	CREATE TABLE IF NOT EXISTS {{.Name}}__events (
		id TEXT PRIMARY KEY,
		created_at INTEGER NOT NULL,
		kind INTEGER NOT NULL,
		pubkey TEXT NOT NULL,
		content TEXT NOT NULL,
		tags TEXT NOT NULL,
		sig TEXT NOT NULL
	);

	CREATE INDEX IF NOT EXISTS {{.Name}}__idx_events_created_at ON {{.Name}}__events(created_at);
	CREATE INDEX IF NOT EXISTS {{.Name}}__idx_events_kind ON {{.Name}}__events(kind);
	CREATE INDEX IF NOT EXISTS {{.Name}}__idx_events_pubkey ON {{.Name}}__events(pubkey);
	CREATE INDEX IF NOT EXISTS {{.Name}}__idx_events_kind_pubkey ON {{.Name}}__events(kind, pubkey);
	CREATE INDEX IF NOT EXISTS {{.Name}}__idx_events_kind_pubkey_created_at ON {{.Name}}__events(kind, pubkey, created_at DESC);

	CREATE TABLE IF NOT EXISTS {{.Name}}__event_tags (
		event_id TEXT NOT NULL,
		key TEXT NOT NULL,
		value TEXT NOT NULL,
		FOREIGN KEY (event_id) REFERENCES {{.Name}}__events(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS {{.Name}}__idx_event_tags_event_id ON {{.Name}}__event_tags(event_id);
	CREATE INDEX IF NOT EXISTS {{.Name}}__idx_event_tags_key ON {{.Name}}__event_tags(key);
	CREATE INDEX IF NOT EXISTS {{.Name}}__idx_event_tags_key_value ON {{.Name}}__event_tags(key, value);
	`)

	if _, err := GetDb().Exec(basicSchema); err != nil {
		return fmt.Errorf("failed to create schema: %w", err)
	}

	// Try to create FTS5 schema - if it fails, continue without it.
	ftsSchema := events.Schema.Render(`
	CREATE VIRTUAL TABLE IF NOT EXISTS {{.Name}}__events_fts USING fts5(
		content,
		content='{{.Name}}__events',
		content_rowid='rowid'
	);

	CREATE TRIGGER IF NOT EXISTS {{.Name}}__events_ai AFTER INSERT ON {{.Name}}__events BEGIN
		INSERT INTO {{.Name}}__events_fts(rowid, content) VALUES (new.rowid, new.content);
	END;

	CREATE TRIGGER IF NOT EXISTS {{.Name}}__events_ad AFTER DELETE ON {{.Name}}__events BEGIN
		INSERT INTO {{.Name}}__events_fts({{.Name}}__events_fts, rowid, content)
		VALUES('delete', old.rowid, old.content);
	END;

	CREATE TRIGGER IF NOT EXISTS {{.Name}}__events_au AFTER UPDATE ON {{.Name}}__events BEGIN
		INSERT INTO {{.Name}}__events_fts({{.Name}}__events_fts, rowid, content)
		VALUES('delete', old.rowid, old.content);
		INSERT INTO {{.Name}}__events_fts(rowid, content)
		VALUES (new.rowid, new.content);
	END;
	`)

	if _, err := GetDb().Exec(ftsSchema); err != nil {
		// FTS5 not available, continue without full-text search
		events.FTSAvailable = false
	} else if _, err := GetDb().Exec(events.Schema.Render(
		`INSERT INTO {{.Name}}__events_fts({{.Name}}__events_fts) VALUES('rebuild');`,
	)); err != nil {
		// Index exists but couldn't be (re)built from existing content - don't
		// advertise/use a search index that might be missing rows.
		events.FTSAvailable = false
	} else {
		// Rebuilding on every Init() is cheap relative to relay startup and keeps
		// the index in sync with any events written before FTS5 became available.
		events.FTSAvailable = true
	}

	return nil
}

func (events *EventStore) Close() {
	// Never close the database, since it's a shared resource
}

func (events *EventStore) QueryEvents(filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
	return events.queryEvents(GetDb(), filter, maxLimit)
}

// queryEvents is the shared implementation behind QueryEvents. It takes an
// explicit runner (a *sql.DB or a *sql.Tx) so callers like ReplaceEvent can
// read within the same transaction as their subsequent writes.
func (events *EventStore) queryEvents(runner squirrel.BaseRunner, filter nostr.Filter, maxLimit int) iter.Seq[nostr.Event] {
	return func(yield func(nostr.Event) bool) {
		if filter.LimitZero {
			return
		}

		// maxLimit is a hard cap the caller wants enforced regardless of what
		// the filter itself asked for - including when the filter didn't ask
		// for a limit at all (filter.Limit == 0, the normal "no limit given"
		// case per NIP-01), which must still be capped.
		if maxLimit > 0 && (filter.Limit == 0 || filter.Limit > maxLimit) {
			filter.Limit = maxLimit
		}

		rows, err := events.buildSelectQuery(filter).RunWith(runner).Query()
		if err != nil {
			return
		}
		defer rows.Close()

		for rows.Next() {
			var evt nostr.Event
			var idStr, pubkeyStr, sigStr, tagsStr string
			var createdAt int64
			var kind int

			err := rows.Scan(&idStr, &createdAt, &kind, &pubkeyStr, &evt.Content, &tagsStr, &sigStr)
			if err != nil {
				continue
			}

			// Parse ID
			if id, err := nostr.IDFromHex(idStr); err == nil {
				evt.ID = id
			} else {
				continue
			}

			// Parse PubKey
			if pubkey, err := nostr.PubKeyFromHex(pubkeyStr); err == nil {
				evt.PubKey = pubkey
			} else {
				continue
			}

			// Parse Signature
			if sigBytes, err := hex.DecodeString(sigStr); err == nil && len(sigBytes) == 64 {
				copy(evt.Sig[:], sigBytes)
			} else {
				continue
			}

			// Set other fields
			evt.CreatedAt = nostr.Timestamp(createdAt)
			evt.Kind = nostr.Kind(kind)

			// Parse Tags
			if err := json.Unmarshal([]byte(tagsStr), &evt.Tags); err != nil {
				continue
			}

			if !yield(evt) {
				return
			}
		}
	}
}

func (events *EventStore) buildSelectQuery(filter nostr.Filter) squirrel.SelectBuilder {
	eventsTable := events.Schema.Prefix("events")

	// Qualify every column with the events table name: once a search join adds
	// the fts5 table below, both tables declare a "content" column and an
	// unqualified reference to it is ambiguous.
	qb := squirrel.Select(
		eventsTable+".id",
		eventsTable+".created_at",
		eventsTable+".kind",
		eventsTable+".pubkey",
		eventsTable+".content",
		eventsTable+".tags",
		eventsTable+".sig",
	).From(eventsTable).OrderBy(eventsTable + ".created_at DESC")

	// Handle search with FTS (if available)
	if filter.Search != "" && events.FTSAvailable {
		ftsTable := events.Schema.Prefix("events_fts")
		// Quote the term as a single FTS5 phrase so arbitrary user input (hyphens,
		// colons, boolean keywords, unbalanced quotes, ...) can't be interpreted
		// as FTS5 query-language syntax and break the MATCH expression.
		phrase := `"` + strings.ReplaceAll(filter.Search, `"`, `""`) + `"`
		qb = qb.Join(fmt.Sprintf("%s ON %s.rowid = %s.rowid", ftsTable, eventsTable, ftsTable)).
			Where(fmt.Sprintf("%s MATCH ?", ftsTable), phrase)
	} else if filter.Search != "" {
		// Fallback to LIKE search if FTS not available
		qb = qb.Where(squirrel.Like{eventsTable + ".content": "%" + filter.Search + "%"})
	}

	if len(filter.IDs) > 0 {
		idStrs := make([]interface{}, len(filter.IDs))
		for i, id := range filter.IDs {
			idStrs[i] = id.Hex()
		}
		qb = qb.Where(squirrel.Eq{"id": idStrs})
	}

	if len(filter.Authors) > 0 {
		authorStrs := make([]interface{}, len(filter.Authors))
		for i, author := range filter.Authors {
			authorStrs[i] = author.Hex()
		}
		qb = qb.Where(squirrel.Eq{"pubkey": authorStrs})
	}

	if len(filter.Kinds) > 0 {
		kindInts := make([]interface{}, len(filter.Kinds))
		for i, kind := range filter.Kinds {
			kindInts[i] = int(kind)
		}
		qb = qb.Where(squirrel.Eq{"kind": kindInts})
	}

	if filter.Since != 0 {
		qb = qb.Where(squirrel.GtOrEq{"created_at": filter.Since})
	}

	if filter.Until != 0 {
		qb = qb.Where(squirrel.LtOrEq{"created_at": filter.Until})
	}

	for tagKey, tagValues := range filter.Tags {
		if len(tagValues) == 0 {
			continue
		}

		if len(tagKey) != 1 {
			continue
		}

		tagValueInterfaces := make([]interface{}, len(tagValues))
		for i, tagValue := range tagValues {
			tagValueInterfaces[i] = tagValue
		}

		subQuery := squirrel.Select("event_id").
			From(events.Schema.Prefix("event_tags")).
			Where(squirrel.Eq{"key": tagKey}).
			Where(squirrel.Eq{"value": tagValueInterfaces})

		subQuerySql, subQueryArgs, _ := subQuery.ToSql()
		qb = qb.Where("id IN ("+subQuerySql+")", subQueryArgs...)
	}

	if filter.Limit > 0 {
		qb = qb.Limit(uint64(filter.Limit))
	}

	return qb
}

func isUniqueConstraintErr(err error) bool {
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique ||
			sqliteErr.ExtendedCode == sqlite3.ErrConstraintPrimaryKey
	}

	return false
}

func (events *EventStore) deleteEvent(runner squirrel.BaseRunner, id nostr.ID) error {
	_, err := squirrel.Delete(events.Schema.Prefix("events")).Where(squirrel.Eq{"id": id.Hex()}).RunWith(runner).Exec()

	return err
}

func (events *EventStore) DeleteEvent(id nostr.ID) error {
	return events.deleteEvent(GetDb(), id)
}

// saveEvent is the shared implementation behind SaveEvent. It relies on the
// events table's primary key to detect duplicates (translating a uniqueness
// violation into eventstore.ErrDupEvent) rather than a separate existence
// check, so there's no check-then-insert window for a concurrent save of the
// same event to race through. The event row and its tag rows are written
// together so a failure never leaves an event whose tags are only partially
// indexed - callers should run this within a transaction (see SaveEvent and
// ReplaceEvent) so a failed tag insert also rolls back the event insert.
func (events *EventStore) saveEvent(runner squirrel.BaseRunner, evt nostr.Event) error {
	tagsJSON, err := json.Marshal(evt.Tags)
	if err != nil {
		return fmt.Errorf("failed to marshal tags: %w", err)
	}

	insertQb := squirrel.Insert(events.Schema.Prefix("events")).
		Columns("id", "created_at", "kind", "pubkey", "content", "tags", "sig").
		Values(
			evt.ID.Hex(),
			int64(evt.CreatedAt),
			int(evt.Kind),
			evt.PubKey.Hex(),
			evt.Content,
			string(tagsJSON),
			hex.EncodeToString(evt.Sig[:]),
		)

	if _, err := insertQb.RunWith(runner).Exec(); err != nil {
		if isUniqueConstraintErr(err) {
			return eventstore.ErrDupEvent
		}

		return fmt.Errorf("failed to save event '%s': %w", evt.ID, err)
	}

	// Insert single-letter tags into event_tags table
	for _, tag := range evt.Tags {
		if len(tag) >= 2 && len(tag[0]) == 1 {
			tagQb := squirrel.Insert(events.Schema.Prefix("event_tags")).
				Columns("event_id", "key", "value").
				Values(evt.ID.Hex(), tag[0], tag[1])

			if _, err := tagQb.RunWith(runner).Exec(); err != nil {
				return fmt.Errorf("failed to save tag %q for event '%s': %w", tag[0], evt.ID, err)
			}
		}
	}

	return nil
}

// runInTx runs fn within a transaction and commits it. SQLite only allows one
// writer at a time; under concurrent writers (e.g. two clients publishing at
// once) fn or the commit itself can fail with a "database is locked"
// (SQLITE_BUSY, including its BUSY_SNAPSHOT variant when a transaction's
// read snapshot goes stale before it upgrades to a write lock) - retrying
// with a brand new transaction, rather than just waiting within the same
// one, is the correct recovery for that variant, so on a busy error the
// whole attempt is retried from scratch with a short backoff instead of
// surfacing a transient error to the caller.
func runInTx(fn func(tx *sql.Tx) error) error {
	const maxAttempts = 5

	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 20 * time.Millisecond)
		}

		var tx *sql.Tx
		if tx, err = GetDb().Begin(); err != nil {
			if isBusyErr(err) {
				continue
			}
			return fmt.Errorf("failed to begin transaction: %w", err)
		}

		if err = fn(tx); err != nil {
			tx.Rollback()
			if isBusyErr(err) {
				continue
			}
			return err
		}

		if err = tx.Commit(); err != nil {
			if isBusyErr(err) {
				continue
			}
			return fmt.Errorf("failed to commit: %w", err)
		}

		return nil
	}

	return fmt.Errorf("gave up after %d attempts: %w", maxAttempts, err)
}

// isBusyErr reports whether err is sqlite reporting that another connection
// currently holds the write lock (SQLITE_BUSY or one of its extended
// variants), as opposed to some other, non-retryable failure.
func isBusyErr(err error) bool {
	var sqliteErr sqlite3.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code == sqlite3.ErrBusy
}

func (events *EventStore) SaveEvent(evt nostr.Event) error {
	return runInTx(func(tx *sql.Tx) error {
		return events.saveEvent(tx, evt)
	})
}

func (events *EventStore) ReplaceEvent(evt nostr.Event) ([]nostr.Event, error) {
	filter := nostr.Filter{Kinds: []nostr.Kind{evt.Kind}, Authors: []nostr.PubKey{evt.PubKey}}
	if evt.Kind.IsAddressable() {
		filter.Tags = nostr.TagMap{"d": []string{evt.Tags.GetD()}}
	}

	var deleted []nostr.Event
	err := runInTx(func(tx *sql.Tx) error {
		shouldSave := true
		shouldDelete := make([]nostr.Event, 0)

		// maxLimit 0 (unbounded) is deliberate: unlike a normal client REQ,
		// this must see every existing event for this kind/author[/d]
		// combination, not just the first one, to correctly clean up if more
		// than one ever exists.
		//
		// Note this deliberately does NOT follow NIP-01's tie-break
		// recommendation of keeping the lowest-id event on an exact
		// created_at tie: zooid uses replaceable/addressable kinds as its
		// own internal, single-writer state store (banned lists, member
		// lists, role assignments, group pins, ...), where two updates
		// landing in the same wall-clock second are common and must resolve
		// to "the one processed last wins" - an admin action that lands in
		// the same second as a previous one must never be silently
		// discarded because of how its id happens to hash.
		for previous := range events.queryEvents(tx, filter, 0) {
			if previous.CreatedAt <= evt.CreatedAt {
				shouldDelete = append(shouldDelete, previous)
			} else {
				shouldSave = false
			}
		}

		if shouldSave {
			if err := events.saveEvent(tx, evt); err != nil && err != eventstore.ErrDupEvent {
				return fmt.Errorf("failed to save: %w", err)
			}
		}

		// Wait until the end to delete old events, just in case our new one doesn't save
		for _, previous := range shouldDelete {
			if err := events.deleteEvent(tx, previous.ID); err != nil {
				return fmt.Errorf("failed to delete replaced event '%s': %w", previous.ID, err)
			}
		}

		deleted = shouldDelete

		return nil
	})

	if err != nil {
		return nil, err
	}

	return deleted, nil
}

func (events *EventStore) CountEvents(filter nostr.Filter) (uint32, error) {
	// Count against every matching row regardless of any pagination Limit the
	// caller set - a Limit is a delivery hint for QueryEvents, not a count cap,
	// and leaving it in place here would double-apply it inside COUNT(*)'s own
	// subquery and silently truncate the result.
	countFilter := filter
	countFilter.Limit = 0

	qb := events.buildSelectQuery(countFilter)

	// Convert the select query to a count query
	countQb := squirrel.Select("COUNT(*)").FromSelect(qb, "subquery")

	var count uint32
	err := countQb.RunWith(GetDb()).QueryRow().Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to count events: %w", err)
	}

	return count, nil
}

// Non-eventstore methods

func (events *EventStore) StoreEvent(event nostr.Event) error {
	if event.Kind.IsReplaceable() || event.Kind.IsAddressable() {
		_, err := events.ReplaceEvent(event)
		return err
	}

	if err := events.SaveEvent(event); err != nil && err != eventstore.ErrDupEvent {
		return err
	}

	return nil
}

func (events *EventStore) SignAndStoreEvent(event *nostr.Event, broadcast bool) error {
	if err := events.Config.Sign(event); err != nil {
		return err
	}

	if err := events.StoreEvent(*event); err != nil {
		return err
	}

	if broadcast && events.Relay != nil {
		events.Relay.BroadcastEvent(*event)
	}

	return nil
}

func (events *EventStore) GetOrCreateApplicationSpecificData(d string) nostr.Event {
	filter := nostr.Filter{
		Kinds: []nostr.Kind{nostr.KindApplicationSpecificData},
		Tags: nostr.TagMap{
			"d": []string{d},
		},
	}

	for event := range events.QueryEvents(filter, 1) {
		return event
	}

	return nostr.Event{
		Kind:      nostr.KindApplicationSpecificData,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			[]string{"d", d},
		},
	}
}

func (events *EventStore) GetOrCreateRelayMembersList() nostr.Event {
	filter := nostr.Filter{
		Kinds: []nostr.Kind{RELAY_MEMBERS},
	}

	for event := range events.QueryEvents(filter, 1) {
		return event
	}

	return nostr.Event{
		Kind:      RELAY_MEMBERS,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			[]string{"-"},
		},
	}
}
