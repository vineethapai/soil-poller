// Command soil-poller periodically checks the latest soil-moisture reading for
// every device in the Tiger Cloud "soil-monitor" database and reports any
// device whose moisture has fallen below a configurable threshold.
//
// Readings are always logged to stdout. If SLACK_WEBHOOK_URL is set, an alert
// summary is also posted to that Slack incoming webhook.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// config holds runtime settings, all sourced from environment variables.
type config struct {
	databaseURL    string        // postgres connection string (required)
	pollInterval   time.Duration // how often to poll
	threshold      float64       // moisture % below which a device alerts
	slackWebhook   string        // optional Slack incoming-webhook URL
	alertOnlySlack bool          // if true, only post to Slack when there are alerts
}

// reading is the latest sample for a single device.
type reading struct {
	DeviceID  int
	Name      string
	FieldZone string
	Time      time.Time
	Moisture  float64
	Temp      float64
}

// latestReadingsQuery returns the most recent reading per device. The
// (device_id, time DESC) index makes the DISTINCT ON efficient.
const latestReadingsQuery = `
SELECT DISTINCT ON (r.device_id)
       r.device_id, d.name, d.field_zone, r.time, r.soil_moisture, r.temperature
FROM soil_readings r
JOIN devices d USING (device_id)
ORDER BY r.device_id, r.time DESC;`

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	// Root context cancelled on SIGINT/SIGTERM for graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		log.Fatalf("failed to create connection pool: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}

	log.Printf("soil-poller started: interval=%s threshold=%.1f%% slack=%t",
		cfg.pollInterval, cfg.threshold, cfg.slackWebhook != "")

	// Poll once immediately, then on every tick.
	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	if err := poll(ctx, pool, cfg); err != nil {
		log.Printf("poll error: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case <-ticker.C:
			if err := poll(ctx, pool, cfg); err != nil {
				log.Printf("poll error: %v", err)
			}
		}
	}
}

// poll runs one cycle: fetch latest readings, log a summary, and alert on any
// device below the moisture threshold.
func poll(ctx context.Context, pool *pgxpool.Pool, cfg config) error {
	// Bound each query so a slow/hung DB can't stall the poller.
	queryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rows, err := pool.Query(queryCtx, latestReadingsQuery)
	if err != nil {
		return fmt.Errorf("query latest readings: %w", err)
	}
	defer rows.Close()

	var readings []reading
	for rows.Next() {
		var r reading
		if err := rows.Scan(&r.DeviceID, &r.Name, &r.FieldZone, &r.Time, &r.Moisture, &r.Temp); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}
		readings = append(readings, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate rows: %w", err)
	}

	var alerts []reading
	for _, r := range readings {
		if r.Moisture < cfg.threshold {
			alerts = append(alerts, r)
		}
	}

	log.Printf("checked %d devices, %d below %.1f%%", len(readings), len(alerts), cfg.threshold)
	for _, r := range alerts {
		log.Printf("  ALERT %-12s zone=%-7s moisture=%.1f%% temp=%.1f°C (as of %s)",
			r.Name, r.FieldZone, r.Moisture, r.Temp, r.Time.Format(time.RFC3339))
	}

	if cfg.slackWebhook != "" && (len(alerts) > 0 || !cfg.alertOnlySlack) {
		if err := postSlack(ctx, cfg.slackWebhook, formatSlack(alerts, len(readings), cfg.threshold)); err != nil {
			return fmt.Errorf("post to slack: %w", err)
		}
	}
	return nil
}

// formatSlack builds the message body sent to Slack.
func formatSlack(alerts []reading, total int, threshold float64) string {
	if len(alerts) == 0 {
		return fmt.Sprintf(":white_check_mark: All %d soil sensors above %.1f%% moisture.", total, threshold)
	}
	var b strings.Builder
	fmt.Fprintf(&b, ":droplet: *%d of %d sensors below %.1f%% moisture:*\n", len(alerts), total, threshold)
	for _, r := range alerts {
		fmt.Fprintf(&b, "• *%s* (%s): %.1f%% moisture, %.1f°C — %s\n",
			r.Name, r.FieldZone, r.Moisture, r.Temp, r.Time.Format("2006-01-02 15:04 MST"))
	}
	return b.String()
}

// postSlack sends a simple text message to a Slack incoming webhook.
func postSlack(ctx context.Context, webhook, text string) error {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, webhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack returned status %d", resp.StatusCode)
	}
	return nil
}

// loadConfig reads configuration from environment variables, applying defaults.
func loadConfig() (config, error) {
	cfg := config{
		databaseURL:    os.Getenv("DATABASE_URL"),
		pollInterval:   5 * time.Minute,
		threshold:      25.0,
		slackWebhook:   os.Getenv("SLACK_WEBHOOK_URL"),
		alertOnlySlack: true,
	}

	if cfg.databaseURL == "" {
		return cfg, fmt.Errorf("DATABASE_URL is required (e.g. from `tiger db connection-string --with-password`)")
	}

	if v := os.Getenv("POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid POLL_INTERVAL %q: %w", v, err)
		}
		cfg.pollInterval = d
	}

	if v := os.Getenv("MOISTURE_THRESHOLD"); v != "" {
		t, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return cfg, fmt.Errorf("invalid MOISTURE_THRESHOLD %q: %w", v, err)
		}
		cfg.threshold = t
	}

	if v := os.Getenv("SLACK_ALERT_ONLY"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return cfg, fmt.Errorf("invalid SLACK_ALERT_ONLY %q: %w", v, err)
		}
		cfg.alertOnlySlack = b
	}

	return cfg, nil
}
