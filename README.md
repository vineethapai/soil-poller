# soil-poller

A small Go service that polls the Tiger Cloud **soil-monitor** database, logs the
latest moisture/temperature reading per device, and alerts (stdout + optional
Slack) when any sensor's moisture drops below a threshold.

## Build

```bash
cd soil-poller
go mod tidy   # fetches github.com/jackc/pgx/v5
go build -o bin/soil-poller .
```

## Configure

Copy the example env file and fill in your connection string:

```bash
cp .env.example .env
# Get a connection string with password:
tiger db connection-string --service-id kw1l0wge3t --with-password
```

| Variable | Required | Default | Description |
|---|---|---|---|
| `DATABASE_URL` | yes | — | Postgres connection string (include the password) |
| `POLL_INTERVAL` | no | `5m` | Go duration between polls (`30s`, `5m`, `1h`) |
| `MOISTURE_THRESHOLD` | no | `25` | Alert when latest moisture < this % |
| `SLACK_WEBHOOK_URL` | no | — | Slack incoming webhook; if unset, logs only |
| `SLACK_ALERT_ONLY` | no | `true` | `false` posts an "all clear" message every poll |

## Run

```bash
export $(cat .env | xargs)
./bin/soil-poller
```

Example output:

```
2026/06/08 10:00:00 soil-poller started: interval=5m0s threshold=25.0% slack=true
2026/06/08 10:00:00 checked 100 devices, 3 below 25.0%
2026/06/08 10:00:00   ALERT sensor-014  zone=zone-B moisture=22.4% temp=19.1°C (as of 2026-06-04T00:00:00Z)
```

## Notes

- The poller reads each device's **latest** raw reading via the
  `(device_id, time DESC)` index, so it works regardless of how fresh the data is.
- It exits cleanly on Ctrl-C / SIGTERM.
- To switch alerting to email, replace `postSlack` in `main.go` with an SMTP
  send (e.g. `net/smtp`) — the alert formatting (`formatSlack`) can be reused.
