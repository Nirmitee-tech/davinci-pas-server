# PAS public run on inferno.healthit.gov via ngrok

The local Inferno (http://localhost:8280) is used during development. The public run repeats the same suites on
https://inferno.healthit.gov so the published results come from the official hosted instance.

Note: the Da Vinci PAS **Server** suites (v2.2.1 and v2.0.1) contain no TLS test group. The public run is not
needed to pass a TLS check; it exists to show the results on the public instance. Still use HTTPS tunnels,
because the public Inferno must reach the server from the internet.

## 1. Prerequisites

- ngrok installed and authenticated (`ngrok config add-authtoken <token>`), a plan that allows two tunnels
  at once, or run Go and .NET one after the other with a single tunnel.
- The PAS server running locally: Go on 8085, .NET on 8086, both passing the local harness run first.
- Outbound HTTPS from the server machine to
  `https://inferno.healthit.gov/suites/custom/subscriptions_r5_backport_r4_server/subscription/channel/notification_listener`
  (Inferno writes this URL into `Subscription.channel.endpoint`; the server posts the handshake and the
  pended event notification there). Check it before starting:
  ```sh
  curl -s -o /dev/null -w "%{http_code}\n" -X POST \
    https://inferno.healthit.gov/suites/custom/subscriptions_r5_backport_r4_server/subscription/channel/notification_listener
  ```
  Any HTTP status (500 is expected when no test is waiting) proves reachability; a timeout does not.
- Auto-finalize left on in the server (`PAS_PENDED_AUTO_FINALIZE_SECONDS`, default 10) as a fallback, and
  notification retries on (every 3 s up to 280 s), so the pended wait resolves even if the harness trigger
  is not used.

## 2. Start the tunnels

If another ngrok agent is already running on the machine (its local API on 127.0.0.1:4040), leave it alone: start a
separate agent per server and stop only that agent's pid. ngrok 3 has no `--web-addr` flag; move the local API of the
new agent with an extra config file merged after the default one:

```sh
mkdir -p harness/reports/ngrok
printf 'version: "2"\nweb_addr: 127.0.0.1:4041\n' > harness/reports/ngrok/agent-web-4041.yml
CFG="$(ngrok config check | sed -n 's/^Valid configuration file at //p')"
ngrok http 8085 --log=stdout --config "$CFG" --config harness/reports/ngrok/agent-web-4041.yml \
  > harness/reports/ngrok/go-8085.log 2>&1 & echo $! > harness/reports/ngrok/go-8085.pid
curl -s http://127.0.0.1:4041/api/tunnels      # public_url of this agent only
```

Use port 4042 and `8086` for .NET. Stop with `kill "$(cat harness/reports/ngrok/go-8085.pid)"`.

Start the PAS server for the run with `PAS_ADMIN_ENABLED=true` and `PAS_PUBLIC_BASE_URL=https://<sub>.ngrok-free.dev`
(Subscription references and `$status` URLs inside notifications use the public host):

```sh
PAS_ADMIN_ENABLED=true PAS_PUBLIC_BASE_URL=https://<sub>.ngrok-free.dev <start command of the Go or .NET server>
```

The `/admin/*` routes are test-only and disabled by default (spec/SPEC.md section 11). Through the tunnel they still
answer 403 because ngrok adds `X-Forwarded-For`; the harness calls them on `--control-url` (localhost). As soon as the
run is finished, stop the tunnel and restart the server without `PAS_ADMIN_ENABLED`.

FHIR bases to give Inferno:

| Server | v2.2.1 | v2.0.1 |
|---|---|---|
| Go | `https://<go-sub>.ngrok-free.app/v221/fhir` | `https://<go-sub>.ngrok-free.app/v201/fhir` |
| .NET | `https://<net-sub>.ngrok-free.app/v221/fhir` | `https://<net-sub>.ngrok-free.app/v201/fhir` |

Sanity check through the tunnel:
```sh
curl -s https://<go-sub>.ngrok-free.app/v221/fhir/metadata | head -c 300
```

The ngrok browser interstitial page only affects browsers. Inferno and curl send non-browser requests and
are not blocked. Keep the tunnels up for the whole run: a restarted free tunnel gets a new hostname, and the
Subscription and notification references already sent would point at the old one.

## 3. Run the harness against the public Inferno

Go first:
```sh
python3 harness/run_suite.py \
  --inferno https://inferno.healthit.gov/suites \
  --suite both \
  --fhir-base "https://<go-sub>.ngrok-free.app/{suite}/fhir" \
  --control-url http://localhost:8085 \
  --label public-go
```

Then .NET:
```sh
python3 harness/run_suite.py \
  --inferno https://inferno.healthit.gov/suites \
  --suite both \
  --fhir-base "https://<net-sub>.ngrok-free.app/{suite}/fhir" \
  --control-url http://localhost:8086 \
  --label public-dotnet
```

`--control-url` stays on localhost: the harness calls `/admin/pended/finalize` directly, not through the
tunnel. Add `--bearer-token <token>` if the server is configured to require a bearer token.

Scripted session creation is accepted by inferno.healthit.gov (confirmed for both PAS server suites). If that ever
changes, run the same inputs manually: create the session in the
browser, paste the files listed in `testdata/<suite>/inputs.json` (the harness `--dry-run` prints the
resolved values, including the FHIR base and notification token), run the groups in order, and when the
pended group waits, call `curl -X POST http://localhost:8085/admin/pended/finalize`.

## 4. What to capture

For each server (Go, .NET) and each suite (v2.2.1, v2.0.1):
- Screenshot of the session overview with every top-level group green (Subscription Setup, Demonstrate
  Workflow Support, Demonstrate Element Support, Demonstrate Error Handling).
- Screenshot of the Successful Pended Workflow group expanded, showing the notification wait and the
  notification conformance tests passing.
- Screenshot of Claim Updates (v2.2.1 only) expanded.
- The public session URL (from the harness summary) and the report folder under `harness/reports/`.
- The ngrok hostname stays in the local report only; replace it with `<ngrok-host>` in any summary you publish.

## 5. After the run

- Stop the tunnels (only the pids you started).
- Restart the servers without `PAS_ADMIN_ENABLED` so the control routes are disabled again.
- Keep the report folders and screenshots with the published results.
