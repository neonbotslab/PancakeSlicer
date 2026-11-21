# MCP agent tools

The Go server provides Streamable HTTP at `/mcp` on its existing listener. The endpoint returns 404 until you enable it. It adds no listener, proxy, or public exposure.

## Configuration

| Environment variable | Default | Behavior |
| --- | --- | --- |
| `ORCA_WEB_MCP_ENABLED` | `false` | Enables `/mcp`. |
| `ORCA_WEB_MCP_TOKEN` | Empty | Requires this additional key as `Authorization: Bearer`. |
| `ORCA_WEB_MCP_CONFIRMATION_TTL` | `5m` | Positive duration for a print approval token. |
| `ORCA_WEB_MCP_ORIGINS` | Empty | Comma-separated exact HTTP or HTTPS origins allowed to send an Origin header. No wildcards or paths. |

The matching command flags are `-mcp-enabled`, `-mcp-token`, and `-mcp-confirmation-ttl`. Origins use the environment variable.

`ORCA_WEB_BEARER_TOKEN` keeps its existing behavior elsewhere and also protects every MCP request when configured. MCP does not turn it on. If both keys are empty, an enabled endpoint uses the server's existing unauthenticated access model.

If both keys have the same value, one Bearer header satisfies both. If they differ, supply the MCP key in Authorization and the existing server's `orca_access` cookie as an additional credential. Obtain that cookie through the existing `POST /api/access` flow with the server key. The MCP key alone never bypasses a configured server key. The MCP key grants no access to REST mutations.

Requests without Origin are allowed for native clients. Every supplied Origin must match the allowlist, including on GET and DELETE. The SDK also retains its localhost DNS-rebinding checks. Do not disable those checks through SDK debug settings.

## Client connection

A client that accepts the common `mcpServers` configuration can use its HTTP transport with this shape. Substitute the existing reachable server URL and configure a key only if the owner has set one.

```json
{
	"mcpServers": {
		"pancakeslicer": {
			"url": "https://slicer.example/mcp",
			"headers": {
				"Authorization": "Bearer replace-with-your-key"
			}
		}
	}
}
```

Keep the same MCP session through preparation and confirmation. A session expires after 15 minutes without HTTP requests. Tools return structured JSON. Tool failures set `isError`.

The client must already be able to reach the server. A local client on the tailnet can use its existing route. A remote cloud client needs an owner-approved route or gateway into that network. Enabling MCP does not create one or change the server's bind address. There is no OAuth discovery or connector provisioning.

## Tools

| Tool | Result or explicit write |
| --- | --- |
| `list_projects` | Active saved projects and revisions. |
| `get_project` | Saved document and source asset references. |
| `list_plates` | Saved plate indexes and object counts. |
| `get_plate` | One saved plate's objects, settings, and events. |
| `list_printers` | Saved setup IDs, live state, progress, AMS trays, and nozzles. |
| `list_profiles` | Saved printer, process, and filament IDs and slot assignments. |
| `list_catalog_filaments` | Filtered and paginated built-in filament products. |
| `get_catalog_profile` | One built-in printer, process, or filament product. |
| `upload_model` | Writes an STL or 3MF from base64 into the content-addressed blob cache. |
| `slice_plate` | Creates a scratch slice job from explicit models, layout, and profile selections. |
| `get_slice_job` | Job status, time, filament grams, warnings, download URL, and transfer status. |
| `prepare_print` | Validates a completed slice and stores a temporary approval record. |
| `confirm_print` | Consumes an approval token and sends and starts its exact prepared job. |

Printer IDs identify saved printer setups. They are not hardware serial numbers. Bambu nozzle extruder index 0 is left and index 1 is right on a dual-nozzle printer. Printer status includes timestamps so clients can distinguish a current report from an offline report. Preparation and confirmation require a fresh online, ready report.

## Slicing inputs

Use `upload_model` with a basename and standard base64 `data`, or upload bytes through the existing `PUT /api/blobs/{sha256}` route. Both paths enforce the server's upload limit. Reference the resulting `name` and `sha256` in `slice_plate.files`. Model upload does not create a saved project.

Choose `library.printer`, `library.print`, ordered `library.filaments`, and `library.slots` from `list_profiles`. `library.project` carries explicit machine, process, and filament overrides. Select `gcode.3mf` output for Bambu LAN printers and `gcode` for other print hosts. Poll `get_slice_job` for the result.

For an STL plate, supply prepared `plate` JSON with its objects and instance placements, together with models baked for that layout. A standalone raw model can omit the layout and use the existing slicer's placement behavior. A native 3MF can select `plate_index` and supply `project_instances`, `project_edits`, and project settings.

For a saved project, also supply `project_id`, `project_revision`, and the selected plate index. These identify the source snapshot. They do not convert the browser's document into engine geometry. Supply the prepared geometry, placements, settings, paint, and edits represented by that snapshot. The tool refuses missing prepared layouts or native instance data instead of silently slicing an original file that might omit saved edits. Arbitrary browser document reconstruction and creating a new saved project from an upload are future work.

Slicing reuses REST profile, geometry, bounds, and queue validation. It writes scratch job files and may update the blob cache. It never saves, replaces, cancels, or changes the revision of an existing project. It does not join a browser slice group.

## Print approval

1. Call `prepare_print` for a completed job and saved printer setup. Set bed leveling, flow calibration, vibration calibration, and timelapse explicitly.
2. Show the human the entire returned summary. It includes printer and connection identity, filename, artifact SHA-256 and size, source project and plate, profile settings, bed type, estimates, warnings, nozzle mapping, AMS mapping, and calibration choices. The summary separates requested choices from `effective_options`; null means the driver does not control that option. Unknown estimates remain null.
3. Obtain explicit approval for that exact summary. Never infer approval from a previous print or from a request to slice.
4. Call `confirm_print` with its token, exact digest, and `human_approved: true` in the same MCP session.

When the client supports form elicitation, confirmation also presents the summary through MCP and requires an accepted form with `approve: true`. Decline, cancel, false, timeout, or protocol failure refuses the print and consumes the token. URL-only elicitation clients cannot confirm prints. Clients without elicitation use the token flow and must obtain approval through their own human interface. The server cannot independently attest that external interaction.

Tokens use cryptographic randomness and expire after the configured lifetime. An owning session's confirmation call with schema-valid arguments consumes its token atomically, including refused calls. Malformed calls rejected by the SDK do not reach the token store. Reuse, a wrong digest, changed options, changed job or printer connection, altered artifacts, stale status, and changed Bambu tray or nozzle topology refuse dispatch. Restarts invalidate pending approvals. A new preparation requires fresh approval.

Pending tokens retain metadata only. Confirmation makes a fresh private copy, checks its SHA-256 against the approved artifact, and uploads that copy through the existing send service. Copies use job scratch space, count toward the disk quota, and remain counted until the upload worker closes and removes them. Preparation removes its copy before returning. Job recovery removes abandoned copies after a restart. It forces a fresh upload rather than trusting a remote filename and file size. Bambu's existing post-upload tray, nozzle, connection, and busy checks still run before print start. Bambu dispatch uses bed type `auto`; the summary separately reports the sliced bed setting. Material mismatch overrides are not exposed through MCP.

A transfer accepted by the server is not proof that the printer started. Poll `get_slice_job` and inspect the transfer outcome. The browser's Device auto-switch follows browser-originated sends and is unchanged.

Pause, resume, printer cancellation, upload cancellation, printer configuration changes, and automatic retries are not MCP tools. Confirmed controls are future work.

## Implementation and verification

The endpoint uses the official `github.com/modelcontextprotocol/go-sdk` v1.6.1. Its maintained Streamable HTTP implementation provides client capability negotiation and form elicitation. The SDK requires Go 1.25 or later. The repository keeps its existing Go toolchain selection and vendors the dependency and its license notices.

The SDK's [license](https://github.com/modelcontextprotocol/go-sdk/blob/v1.6.1/LICENSE) covers Apache-2.0 and retained MIT contributions. Those licenses are compatible with AGPL-3.0. The [GNU licensing FAQ](https://www.gnu.org/licenses/gpl-faq.en.html) applies the GPLv3 compatibility guidance to AGPLv3. PancakeSlicer remains AGPL-3.0 with its existing upstream attribution and Corresponding Source obligations.

Run `go vet -mod=vendor ./...` and `go test -mod=vendor -count=1 ./...` from `server`. The MCP tests use the SDK client over HTTP, temporary workspace and job storage, a fixture slicing executable, and local mock printers. They do not need a slicer engine or printer credentials.
