package server

import (
	"context"
	"database/sql"
	"time"
)

type deliveryEvent struct {
	State   string `json:"state"`
	Address string `json:"address,omitempty"`
	At      int64  `json:"at"`
	Due     int64  `json:"due,omitempty"`
	Code    int    `json:"code,omitempty"`
	Route   string `json:"route,omitempty"`
}

func initDeliveryTimeline(ctx context.Context, db *sql.DB, driver string) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS delivery_events (id INTEGER PRIMARY KEY AUTOINCREMENT, outbox_id INTEGER NOT NULL,
			owner TEXT NOT NULL, recipient TEXT NOT NULL, state TEXT NOT NULL,
			stamp INTEGER NOT NULL, due INTEGER NOT NULL, smtp_code INTEGER NOT NULL, route TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS delivery_events_job ON delivery_events(owner,outbox_id,id)`,
		`CREATE INDEX IF NOT EXISTS delivery_events_stamp ON delivery_events(stamp,id)`,
	}
	if driver == "mysql" {
		statements = []string{`CREATE TABLE IF NOT EXISTS delivery_events (
			id BIGINT PRIMARY KEY AUTO_INCREMENT, outbox_id BIGINT NOT NULL,
			owner VARCHAR(254) NOT NULL, recipient VARCHAR(254) NOT NULL,
			state VARCHAR(24) NOT NULL, stamp BIGINT NOT NULL, due BIGINT NOT NULL, smtp_code INT NOT NULL,
			route VARCHAR(8) NOT NULL DEFAULT '',
			KEY delivery_events_job (owner,outbox_id,id),
			KEY delivery_events_stamp (stamp,id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`}
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return ensureColumn(ctx, db, driver, "delivery_events", "route", "TEXT NOT NULL DEFAULT ''", "VARCHAR(8) NOT NULL DEFAULT ''")
}

func recordDeliveryEvent(writer outboxWriter, owner string, id int64, recipient, state string, code int, due int64) error {
	return recordDeliveryEventWithRoute(writer, owner, id, recipient, state, code, due, "")
}

func recordDeliveryEventWithRoute(writer outboxWriter, owner string, id int64, recipient, state string, code int, due int64, route string) error {
	_, err := writer.Exec(`INSERT INTO delivery_events(outbox_id,owner,recipient,state,stamp,due,smtp_code,route)
		VALUES(?,?,?,?,?,?,?,?)`, id, owner, recipient, state, time.Now().Unix(), due, code, route)
	return err
}

func (a *app) pruneDeliveryTimeline(ctx context.Context) error {
	cutoff := time.Now().Add(-90 * 24 * time.Hour).Unix()
	for {
		result, err := a.db.ExecContext(ctx, `DELETE FROM delivery_events WHERE id IN (
			SELECT id FROM (SELECT id FROM delivery_events WHERE stamp < ? ORDER BY stamp,id LIMIT 1000) AS expired
		)`, cutoff)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count < 1000 {
			return err
		}
	}
}

func readDeliveryTimeline(ctx context.Context, db recipientQueryer, owner string, id int64) ([]deliveryEvent, error) {
	rows, err := db.QueryContext(ctx, `SELECT recipient,state,stamp,due,smtp_code,route FROM delivery_events
		WHERE owner=? AND outbox_id=? ORDER BY id DESC LIMIT 200`, owner, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []deliveryEvent
	for rows.Next() {
		var event deliveryEvent
		if err := rows.Scan(&event.Address, &event.State, &event.At, &event.Due, &event.Code, &event.Route); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
