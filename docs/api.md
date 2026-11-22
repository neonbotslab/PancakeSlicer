# Printer API

The printer API reads saved printer connections. It does not upload files, send G-code, control jobs, or change printer settings. Existing printer controls remain separate.

## Authentication

When `ORCA_WEB_BEARER_TOKEN` is set, all endpoints below require `Authorization: Bearer <token>`. The existing same-origin access cookie also works. Query-string tokens are not accepted. When the token is unset, these endpoints allow access without authentication, consistent with the server's local access configuration. Responses use `Cache-Control: no-store`.

The printer ID is the saved setup ID returned by `GET /api/printers/status`. IDs in URLs must be percent-encoded. Responses exclude printer serials, network addresses, credentials, spool UUIDs, and server file paths.

## Endpoints

| Method | Path | Response |
| --- | --- | --- |
| GET | `/api/printers/status` | `{ "schema_version": 1, "printers": [...] }` |
| GET | `/api/printers/{id}/status` | One printer status object with the same schema as a list item |
| GET | `/api/printers/{id}/cameras` | `{ "cameras": [{ "id": "0", "name": "Chamber camera" }] }` |
| GET | `/api/printers/{id}/camera/stream.mjpeg` | `multipart/x-mixed-replace; boundary=orca-camera` |
| GET | `/api/printers/{id}/camera/snapshot.jpg` | One `image/jpeg` frame |

Camera requests accept `?camera={camera_id}`. Omit the selector to use the first discovered camera. Camera IDs come from the cameras endpoint. H2D currently exposes the existing LAN chamber feed as camera `0`. Additional physical cameras are not advertised without a supported LAN source. Moonraker's discovered webcams can be selected individually.

Unknown printer IDs and camera IDs return HTTP 404. Missing or invalid credentials return 401 before monitoring or streaming begins. A conflicting active camera selection or a full JPEG subscriber limit returns 503 after a bounded wait. Changed printer configuration returns 409. Source discovery failure returns 502. No first frame within 30 seconds returns 504. A stream that has already started closes on timeout or removal. Clients can reconnect after it closes.

## Status schema version 1

Every field below is present. Unknown scalar values are `null`. Unknown or empty collections are `[]`. Existing state names can expand without changing the schema version. Removing fields, changing their meaning, or changing units requires a new schema version.

| Field | Type | Meaning |
| --- | --- | --- |
| `schema_version` | integer | `1` |
| `id`, `name` | string | Saved printer setup ID and display name |
| `state` | string | Normalized state, including `unavailable`, `connecting`, `offline`, `idle`, `busy`, `printing`, `paused`, `finished`, and `failed` |
| `online` | boolean | The current monitor has received usable status and has not reported a connection failure |
| `connected` | boolean | The monitor's current link state is online. This does not guarantee fresh telemetry |
| `progress` | number or null | Fraction between 0 and 1 |
| `layer.current`, `layer.total` | integer or null | Reported current and total layers |
| `temps.nozzles` | array | `{ "id": 0, "side": "right", "actual": 208, "target": 210 }`. Temperatures are degrees Celsius. H2D wire IDs are 0 for right and 1 for left. An unspecified side is an empty string |
| `temps.bed`, `temps.chamber` | object or null | `{ "actual": 60, "target": 60 }` in degrees Celsius. The existing monitor uses target 0 when the printer supplies only actual temperature |
| `remaining_seconds` | integer or null | Reported remaining duration in seconds |
| `job.file` | string or null | Current filename, with directory components removed |
| `job.started_at` | string or null | Reported job start time, in UTC RFC 3339 format |
| `ams.units` | array | AMS units, described below |
| `ams.external_trays` | array | External spool trays using the tray schema below |
| `errors` | array | Objects with `code`. Free-form upstream messages are excluded. Includes printer errors and Bambu HMS codes such as `00000001-00000002` |
| `updated_at` | string or null | UTC RFC 3339 time of the most recent accepted telemetry report, including an unchanged report |
| `age_seconds` | number or null | Nonnegative seconds since `updated_at`, calculated when serving the response |
| `stale` | boolean | True before any usable report or when the last report is at least 30 seconds old |

An AMS unit contains `id`, `kind`, `extruder`, `humidity_percent`, `humidity_level`, and `trays`. `id` is numeric. `kind` is the existing decoded AMS family, such as `ams`, `ams-2-pro`, or `ams-ht`. `extruder` is an integer or null. H2D AMS extruder bindings use logical IDs 0 for left and 1 for right, distinct from nozzle temperature wire IDs. Humidity percentage comes from the printer's `humidity_raw` value when reported. The older `humidity` field remains a separate level from 1 through 5. Missing humidity is null, and levels are not converted into percentages.

Each tray contains numeric `id` and `slot`, nullable integer `extruder`, string `type`, string `color`, nullable integer `remaining_percent`, and boolean `active`. Colors use `#RRGGBB`, or an empty string when unknown. External tray IDs retain the printer protocol IDs. AMS unit and tray arrays are sorted by ID and slot.

The cache merges partial reports. `updated_at` measures receipt of a usable telemetry report, not a new observation of every individual field. A cold or unsupported connection can return `unavailable` with null values. A connection failure can clear current measurements while preserving the last report time. Read `state`, `online`, `connected`, and `stale` together.

Status GETs wait up to three seconds for an initial observation. They renew passive monitoring for one minute for the requested printer, or all printers for the list endpoint. Passive Bambu monitoring sends MQTT CONNECT, SUBSCRIBE, and keepalive packets. It never publishes full-status requests or printer commands. A cold cache waits for unsolicited status that includes a print state. The existing UI and legacy MCP tools retain their own monitoring behavior, including status requests. API polling does not extend their monitoring grace period.

A complete synthetic response with private identifiers replaced is saved in [printer-status.redacted.json](examples/printer-status.redacted.json).

## Camera transport and limits

MJPEG parts each contain `Content-Type: image/jpeg` and `Content-Length`, followed by a complete JPEG. The snapshot endpoint joins the same feed and returns one frame. HEAD returns the response type without opening the upstream camera.

The server shares one upstream camera connection per printer across the existing browser stream, API streams, and snapshots. Distinct camera selections serialize through that connection. A selector cannot interrupt viewers of another camera. When no viewers remain, the upstream closes after a three-second grace period. Upstream failures reconnect with exponential delays from 250 milliseconds to 30 seconds while JPEG viewers remain. Native MP4 viewers reconnect through their existing browser behavior.

Each JPEG client keeps one pending frame. New frames replace the pending frame for slow readers. There are at most 64 JPEG subscribers per printer. A cached JPEG can seed a new client only while it is younger than two seconds. JPEG frames are limited to 4 MiB. Ordered MP4 fragments and initialization data are each limited to 32 MiB. The shared decoder input holds at most two chunks. Backpressure restarts the JPEG decoder without opening another printer connection. HTTP writes have a ten-second deadline. Thirty seconds without a frame closes an active API stream.

For H2D and other RTSPS sources, `ffmpeg` must be installed on the server. The existing MP4 stream remains available to the browser. A single additional decoder starts when JPEG consumers join and reads that MP4 locally. It outputs at most five frames per second and scales widths above 1280 pixels. Its stderr is not exposed. JPEG decoding stops after the final JPEG consumer leaves. HLS is not provided.

## MCP

When the existing `/mcp` service is enabled, its read-only `get_printer_status` tool accepts an optional `printer` ID. It returns `{ "schema_version": 1, "printers": [...] }` with the same projection and passive monitoring as the HTTP API. Existing MCP authentication and origin rules apply.
