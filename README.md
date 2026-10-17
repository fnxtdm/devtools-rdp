# devtools-rdp

Expose TCP ports of one or more Windows machines reached over RDP on your local
machine, using the RDP connection itself as the transport. Typical use: a browser
already listening for CDP on `127.0.0.1:<port>` becomes `http://127.0.0.1:<port>`
locally.

The remote side needs no SSH server, no open ports and no pre-installed
software: `devtools-rdp` logs into each RDP session, types a short bootstrap line
into PowerShell, pushes a native Go agent through a custom RDP virtual channel
and starts it. **Chrome is not managed by the tool** — the agent only forwards
TCP, so whatever already listens on the remote host (Chrome, BitBrowser, Edge,
any service) is what you reach.

```
 script ──> 127.0.0.1:49885 ──> devtools-rdp ──> RDP channel ──> agent ──> 127.0.0.1:49885
 script ──> 127.0.0.1:54283 ──┘      (target A)                       (machine A)
                                  (target B)  ──────────────────────>  (machine B)
```

## How it works

1. `devtools-rdp` connects with RDP + NLA (NTLMv2) and requests a custom static
   virtual channel (`rdpdt` by default).
2. It opens PowerShell in the interactive desktop via synthetic keystrokes and
   types one gzip+base64 line (~1.4 KB) that decompresses a small C# loader.
3. The loader opens the virtual channel, prints `READY`, receives the agent
   config and a 3 MB native Windows executable, writes them to `%TEMP%` and
   starts the agent. It then closes its channel handle.
4. `devtools-rdp-agent.exe` reopens the channel and bridges framed `OPEN`/`DATA`/
   `CLOSE` messages to TCP sockets on the Windows side.
5. Every local TCP connection to a configured local port becomes a stream on
   that target's channel; the agent dials `remote_host:remote_port` inside the
   RDP session. One process can drive several targets at once, each with its own
   RDP connection, agent and mapping table.

No clipboard, no file shares, no additional listeners on the Windows host.

## Requirements

- Local machine: Go 1.21+ to build (macOS/Linux/Windows).
- RDP host: Windows 10/11 with PowerShell 5.1 (default) and an account that can
  log into the session.
- Something already listening on the remote port you want to reach (for CDP:
  start Chrome/BitBrowser yourself with `--remote-debugging-port=<port>`).
- The bootstrap types into the desktop, so the session must be free of modal
  dialogs while it runs.

## Build

```bash
cd ~/devtools-rdp
make all          # dist/devtools-rdp and dist/devtools-rdp-agent.exe
```

`dist/devtools-rdp` looks for the agent next to itself, in `../dist/` or in `./dist/`.
You can also pass `-agent /path/to/devtools-rdp-agent.exe`.

## Configure

Copy `config.example.json` to `config.json` and list one entry per RDP host:

```json
{
  "targets": [
    {
      "name": "cloud",
      "rdp": {
        "host": "cloudpc-rdp.bitbrowser.net",
        "port": 40024,
        "username": "admin",
        "password": "...",
        "domain": "",
        "width": 1280,
        "height": 800
      },
      "channel_name": "rdpdt",
      "listen_host": "127.0.0.1",
      "forwards": [
        { "local_port": 9222,  "remote_port": 9222 },
        { "local_port": 49885, "remote_port": 49885 }
      ]
    },
    {
      "name": "lan",
      "rdp": { "host": "192.168.0.39", "port": 3389, "username": "bit", "password": "..." },
      "forwards": [
        { "local_port": 54283, "remote_port": 54283 }
      ]
    }
  ]
}
```

- Each target gets its own RDP connection, agent and listeners; tasks run in
  parallel, so N machines cost one bootstrap round (~40 s) rather than N.
- `name` is used in logs, in `-only` and in the control endpoint.
- `forwards[].remote_host` (or the target-level `remote_host`) defaults to
  `127.0.0.1`; set it if the service lives on another host reachable from the
  RDP machine.
- `local_port` must be unique across all targets (they all bind locally).
- `channel_name` must be 7 characters or fewer; keep the default unless you need
  several clients on the same host.
- There is no `chrome` block any more: the agent no longer starts or inspects a
  browser, it only forwards TCP. Start Chrome/BitBrowser on the remote host
  yourself and put its `--remote-debugging-port` in `forwards`.

## Run

```bash
./dist/devtools-rdp -c config.json                 # every target
./dist/devtools-rdp -c config.json -only lan       # one target (or lan,cloud)
```

Each target logs with a `[name]` prefix:

```
[cloud] connecting to cloudpc-rdp.bitbrowser.net:40024 as admin
[lan]   connecting to 192.168.0.39:3389 as bit
[lan]   RDP connected
[lan]   forwarding 127.0.0.1:54283 -> 127.0.0.1:54283
[lan]   agent: {"agent":"devtools-rdp","version":2}
[lan]   ready: http://127.0.0.1:54283 -> remote 127.0.0.1:54283
```

Flags:

- `-only name[,name]` — start a subset of targets.
- `-forward name:local[:remote[:host]]` — add a mapping at startup without
  editing the config (the target name may be omitted when there is one target).
- `-no-bootstrap` — do not touch the desktop; expect an agent that is already
  running.
- `-timeout 150s` — how long each target may take to hand over the agent.
- `-agent PATH` — explicit path to `devtools-rdp-agent.exe`.
- `-control 127.0.0.1:9221` — control endpoint; `""` disables it.
- `-run '<command>'` — type one PowerShell command into the target desktop and
  exit (see below).

### Run a command on the remote desktop

`-run` logs in, opens PowerShell in the interactive desktop, types one command
and disconnects. It is meant for one-off setup the tunnel does not cover — for
example starting a browser with a CDP port. It is also handy for A/B baselines:
start a stock Chrome next to a managed browser so both share the same machine
and the same egress IP.

```bash
./dist/devtools-rdp -c config.json -only cloud -run \
  '$p=(Get-Command chrome.exe -EA SilentlyContinue).Source; if(-not $p){$p="$env:ProgramFiles\Google\Chrome\Application\chrome.exe"}; Start-Process $p -ArgumentList "--remote-debugging-port=9992","--user-data-dir=C:\cdp9992","--no-first-run"'
```

Then expose the port as usual (`curl -X POST '.../forward?target=cloud&listen=9992'`).

Keep in mind:

- **It takes over the RDP session.** Stop the running client first (`pkill -f
  dist/devtools-rdp`); two clients cannot share one interactive session. Start
  the tunnel again afterwards.
- **GUI processes survive the disconnect.** Anything started with `Start-Process`
  keeps running in the session after `-run` exits, which is what makes this
  useful.
- It is pure keystroke injection, so the desktop must be unlocked and free of
  modal dialogs. On slow hosts raise `rdp.type_delay_ms` (default 8 ms/char) or
  characters get dropped — a 1.4 KB loader line is far more sensitive to this
  than a short command.
- The command is typed into PowerShell, so quoting follows PowerShell rules, not
  the local shell's.
- `-run` needs exactly one target: pass `-only NAME`. Give any browser you start
  a dedicated `--user-data-dir` — Chrome 136+ refuses `--remote-debugging-port`
  on the default profile.

### Add a port without restarting

New browser windows come and go, so forwards can be added while running:

```bash
curl -X POST 'http://127.0.0.1:9221/forward?target=cloud&listen=58528'
curl -X POST 'http://127.0.0.1:9221/forward?target=cloud&listen=58528&remote=9222'
curl           'http://127.0.0.1:9221/forward'
```

`target` may be omitted when only one target is configured. The endpoint binds
to localhost only.

## Verify

```bash
curl http://127.0.0.1:9222/json/version
curl http://127.0.0.1:9222/json/list
python3 examples/cdp_check.py
python3 examples/cdp_probe.py https://example.com   # network/DOM/bot-wall report
python3 examples/cdp_probe.py --ip-only             # egress IP + fingerprint
python3 examples/fp_compare.py http://127.0.0.1:9222 http://127.0.0.1:51110
python3 examples/cdp_audit.py http://127.0.0.1:9222   # PASS/FAIL bot-wall readiness
```

Playwright:

```python
from playwright.sync_api import sync_playwright

with sync_playwright() as p:
    browser = p.chromium.connect_over_cdp("http://127.0.0.1:9222")
    page = browser.contexts[0].pages[0]
    print(page.title())
```

## Troubleshooting

- **`loader did not report in`** — the keystroke sequence did not reach a
  PowerShell prompt. Common causes: the session is locked (the third attempt
  sends Ctrl+Alt+Del and the password), a UAC/modal dialog has focus, or the
  desktop is still on the welcome screen. Retry; the bootstrap takes ~40 s.
- **Agent never says hello** — check `%TEMP%\devtools-rdp-agent.log` on the Windows
  host (open it in the RDP session). The loader writes the agent and config to
  `%TEMP%\devtools-rdp-agent.json` and a uniquely named `devtools-rdp-agent-*.exe`.
- **`Connection refused` on a forwarded port** — nothing is listening on
  `remote_host:remote_port` inside the RDP session. Start the browser with
  `--remote-debugging-port=<port>` (Chrome 136+ rejects remote debugging on the
  default profile, so pass a dedicated `--user-data-dir`).
- **Connection dies** — the tunnel lives inside the RDP session; if the RDP
  connection drops, the agent goes with it.

## Notes

- `third_party/grdp` is a vendored copy of `github.com/tomatome/grdp` with two
  small patches: `protocol/t125/mcs.go` no longer imports the Windows-only
  clipboard plugin and exposes `SetClientStaticChannel`, which is what lets the
  client request the custom channel. The agent reads the channel in
  non-blocking mode because `WTSVirtualChannelRead` with an infinite timeout
  returns `ERROR_IO_INCOMPLETE` after the first read on this host.
- The local listener binds to `127.0.0.1`. CDP grants full control over the
  browser profile, so do not expose it publicly.
- `config.json` contains the RDP password and is git-ignored.
