package demo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
)

// Kafka produces the evse_status event the roaming consumer listens on. From
// outside the cluster every redpanda hostname resolves to one LB IP that lands
// on broker 0, so only partitions led by broker 0 are writable — the partition
// is pinned by config and its leader dialed directly (DialLeader), and a
// startup probe downgrades the feature to disabled instead of failing later.
type Kafka struct {
	cfg     Config
	enabled bool
	reason  string
}

func NewKafka(cfg Config) *Kafka {
	k := &Kafka{cfg: cfg}
	if !cfg.KafkaEnabled {
		k.reason = "disabled by config (DEMO_ENABLE_KAFKA or DEMO_KAFKA_* missing)"
		return k
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := kafka.DialLeader(ctx, "tcp", cfg.KafkaBroker, cfg.KafkaTopic, cfg.KafkaPartition)
	if err != nil {
		k.reason = fmt.Sprintf("partition %d leader unreachable: %v", cfg.KafkaPartition, err)
		return k
	}
	conn.Close()
	k.enabled = true
	return k
}

func (k *Kafka) Enabled() (bool, string) { return k.enabled, k.reason }

type evseStatusEvent struct {
	EvseUID     string `json:"evse_uid"`
	EvseID      string `json:"evse_id"`
	StationID   string `json:"station_id"`
	OCPPStatus  string `json:"ocpp_status"`
	LastUpdated string `json:"last_updated"`
}

func (k *Kafka) ProduceEvseStatus(ctx context.Context, status string) error {
	if !k.enabled {
		return fmt.Errorf("kafka demo disabled: %s", k.reason)
	}
	payload, err := json.Marshal(evseStatusEvent{
		EvseUID:     k.cfg.KafkaEvseUID,
		EvseID:      k.cfg.KafkaEvseID,
		StationID:   k.cfg.KafkaStationID,
		OCPPStatus:  status,
		LastUpdated: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	})
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, err := kafka.DialLeader(dialCtx, "tcp", k.cfg.KafkaBroker, k.cfg.KafkaTopic, k.cfg.KafkaPartition)
	if err != nil {
		return fmt.Errorf("dial leader: %w", err)
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if _, err := conn.WriteMessages(kafka.Message{
		Key:   []byte(k.cfg.KafkaStationID),
		Value: payload,
	}); err != nil {
		return fmt.Errorf("produce: %w", err)
	}
	return nil
}
