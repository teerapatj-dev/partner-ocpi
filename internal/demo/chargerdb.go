package demo

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ChargerDB is the demo-only lever that makes the EVSE status flow show a real value instead of the
// UNKNOWN a dev charger always reports. Evolt derives the pushed status from the charger's own DB row
// (core-charger's evses/chargers), and a dev charger is connectivity_status='offline', which
// effectiveEvseStatus forces to UNKNOWN before it ever looks at ocpp_status. So the demo pretends the
// one configured EVSE is online and carries the status the operator picked.
//
// Every write is pinned to the single configured EVSE id — the client holds no method that can touch
// any other row, so a bad request cannot walk the charger table.
type ChargerDB struct {
	pool   *pgxpool.Pool
	evseID string
}

func NewChargerDB(ctx context.Context, dsn, evseID string) (*ChargerDB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("charger db connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("charger db ping: %w", err)
	}
	return &ChargerDB{pool: pool, evseID: evseID}, nil
}

func (c *ChargerDB) Close() {
	if c != nil && c.pool != nil {
		c.pool.Close()
	}
}

// SimulateOnline sets the demo EVSE to a state effectiveEvseStatus will map straight through to the
// given OCPP status: connectivity online and operative clear (both gates that would otherwise win),
// and ocpp_status to the target. It returns the charger id it touched so the caller can report the
// row it changed. Scoped to c.evseID — the charger update is by that evse's charger_id only.
func (c *ChargerDB) SimulateOnline(ctx context.Context, ocppStatus string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var chargerID string
	err = tx.QueryRow(ctx,
		`UPDATE evses SET ocpp_status = $1, status = 'ready', updated_at = now()
		 WHERE id = $2 RETURNING charger_id`, ocppStatus, c.evseID).Scan(&chargerID)
	if err != nil {
		return "", fmt.Errorf("update evse %s: %w", c.evseID, err)
	}

	// operative_status='ready' and connectivity='online' are the two gates effectiveEvseStatus checks
	// before ocpp_status; 'ready' is the charger_operative_status label that falls through (the others
	// map to OUTOFORDER/INOPERATIVE and would mask the picked status).
	if _, err := tx.Exec(ctx,
		`UPDATE chargers SET connectivity_status = 'online', operative_status = 'ready', updated_at = now()
		 WHERE id = $1`, chargerID); err != nil {
		return "", fmt.Errorf("update charger %s: %w", chargerID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return chargerID, nil
}
