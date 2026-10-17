"""Probe a page through the tunnel: navigation, network, challenge frames, environment.

Usage:
    python3 examples/cdp_probe.py [url] [--wait 30] [--keep]

Answers "why is this page blocked / blank / stuck" by dumping the redirect
chain, non-2xx API calls with their bodies, bot-challenge iframes, the rendered
text, the egress IP of the remote machine and the fingerprint a bot detector
sees.
"""
import argparse
import asyncio
import json
import sys
import urllib.request

import websockets

ENDPOINT = "http://127.0.0.1:9222"
DEFAULT_URL = (
    "https://www.traveloka.com/en-en/flight/fullsearch"
    "?ap=CGK.DPS&dt=30-10-2026.NA&ps=1.0.0&sc=ECONOMY"
)
CHALLENGE_HOSTS = ("captcha-delivery.com", "challenges.cloudflare.com", "hcaptcha.com",
                   "recaptcha", "perimeterx", "px-cdn", "arkoselabs")
BLOCKED_PATTERNS = ["*.png", "*.jpg", "*.jpeg", "*.webp", "*.gif", "*.avif", "*.svg",
                    "*.woff", "*.woff2", "*.ttf", "*.otf", "*.mp4", "*.webm", "*.m3u8"]

SNAPSHOT_JS = """(() => ({
  href: location.href,
  title: document.title,
  readyState: document.readyState,
  htmlLength: document.documentElement.outerHTML.length,
  text: (document.body ? document.body.innerText : '').slice(0, 4000),
  iframes: [...document.querySelectorAll('iframe')].map(f => f.src),
  fingerprint: {
    webdriver: navigator.webdriver,
    chrome: !!window.chrome,
    languages: navigator.languages,
    plugins: navigator.plugins.length,
    tz: Intl.DateTimeFormat().resolvedOptions().timeZone,
    tzOffsetMin: new Date().getTimezoneOffset(),
    ua: navigator.userAgent,
    platform: navigator.platform,
    cores: navigator.hardwareConcurrency,
    memory: navigator.deviceMemory,
    screen: [screen.width, screen.height, devicePixelRatio],
    webgl: (() => {
      const gl = document.createElement('canvas').getContext('webgl');
      const dbg = gl && gl.getExtension('WEBGL_debug_renderer_info');
      return dbg ? gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL) : null;
    })(),
  },
}))()"""


class Client:
    def __init__(self, ws):
        self.ws = ws
        self._id = 0
        self._pending = {}
        self.events = []
        self._reader = asyncio.create_task(self._read())

    async def _read(self):
        async for raw in self.ws:
            msg = json.loads(raw)
            if "id" in msg:
                fut = self._pending.pop(msg["id"], None)
                if fut and not fut.done():
                    fut.set_result(msg)
            else:
                self.events.append(msg)

    async def call(self, method, params=None, session=None, timeout=30):
        self._id += 1
        msg = {"id": self._id, "method": method, "params": params or {}}
        if session:
            msg["sessionId"] = session
        fut = asyncio.get_running_loop().create_future()
        self._pending[self._id] = fut
        await self.ws.send(json.dumps(msg))
        try:
            reply = await asyncio.wait_for(fut, timeout)
        except asyncio.TimeoutError:
            print(f"!! {method} timed out after {timeout}s", file=sys.stderr)
            return {"result": {}}
        if "error" in reply:
            print(f"!! {method} error: {reply['error']}", file=sys.stderr)
        return reply

    async def evaluate(self, expression, session, timeout=15):
        reply = await self.call(
            "Runtime.evaluate",
            {"expression": expression, "returnByValue": True, "awaitPromise": True},
            session, timeout)
        return reply.get("result", {}).get("result", {}).get("value")


def show_events(events):
    console, failed = [], []
    for e in events:
        method, p = e["method"], e["params"]
        if method == "Network.requestWillBeSent":
            rr = p.get("redirectResponse")
            if rr:
                print(f'  {rr["status"]:>4} redirect  {rr["url"][:120]}')
        elif method == "Network.responseReceived":
            r = p["response"]
            kind = p.get("type")
            if kind == "Document":
                print(f'  {r["status"]:>4} document  {r["mimeType"][:28]:28} '
                      f'server={r["headers"].get("server", "")[:18]:18} {r["url"][:110]}')
            elif kind in ("XHR", "Fetch"):
                print(f'  {r["status"]:>4} {kind:8}  {r["url"][:120]}')
        elif method == "Network.loadingFailed":
            if p.get("blockedReason"):
                failed.append(p)
        elif method == "Page.frameNavigated":
            f = p["frame"]
            if not f.get("parentId"):
                print(f'  nav       {f["url"][:120]}')
        elif method == "Log.entryAdded":
            en = p["entry"]
            if en.get("level") == "error":
                console.append(f'[{en.get("source")}] {en.get("text", "")[:400]}')
        elif method == "Runtime.exceptionThrown":
            d = p.get("exceptionDetails", {})
            console.append(f'[exception] {d.get("text", "")} '
                           f'{d.get("exception", {}).get("description", "")[:300]}')
    if failed:
        print("\n  blocked requests:")
        for p in failed:
            print(f'    {p.get("type", "?"):10} {p.get("errorText")} '
                  f'blocked={p.get("blockedReason")} {p.get("requestId")}')
    if console:
        print("\n  console errors:")
        for line in console[:25]:
            print("    " + line.replace("\n", " ")[:300])


async def fetch_egress_ip(c):
    target = (await c.call("Target.createTarget", {"url": "https://ipinfo.io/json"}))["result"]["targetId"]
    session = (await c.call("Target.attachToTarget",
                            {"targetId": target, "flatten": True}))["result"]["sessionId"]
    body = None
    for _ in range(40):
        await asyncio.sleep(0.5)
        try:
            body = await c.evaluate("document.body ? document.body.innerText : ''", session, timeout=5)
        except Exception:
            body = None
        if body and body.strip().startswith("{"):
            break
    await c.call("Target.closeTarget", {"targetId": target})
    print("\n=== remote egress ===")
    print(body.strip()[:800] if body else "(no answer)")


async def dump_challenge_frames(c):
    targets = (await c.call("Target.getTargets")).get("result", {}).get("targetInfos", [])
    for t in targets:
        if t["type"] != "iframe":
            continue
        if not any(h in t["url"] for h in CHALLENGE_HOSTS):
            continue
        info = await c.call("Target.attachToTarget", {"targetId": t["targetId"], "flatten": True})
        sid = info.get("result", {}).get("sessionId")
        if not sid:
            continue
        await c.call("Runtime.enable", {}, sid)
        text = await c.evaluate(
            "JSON.stringify({text:(document.body?document.body.innerText:'').slice(0,600),"
            "html:document.documentElement.outerHTML.length})", sid)
        print(f'  iframe {t["url"][:90]}')
        print(f'    {str(text)[:700]}')
        await c.call("Target.detachFromTarget", {"sessionId": sid})


async def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("url", nargs="?", default=DEFAULT_URL)
    ap.add_argument("--wait", type=int, default=30, help="seconds to collect events")
    ap.add_argument("--keep", action="store_true", help="leave the tab open")
    ap.add_argument("--no-ip", action="store_true", help="skip egress IP lookup")
    ap.add_argument("--all-assets", action="store_true",
                    help="load images and fonts too (heavy: floods the tunnel)")
    ap.add_argument("--ip-only", action="store_true",
                    help="only report the remote egress IP and browser fingerprint")
    ap.add_argument("--endpoint", default=ENDPOINT,
                    help="CDP endpoint, e.g. http://127.0.0.1:51110")
    args = ap.parse_args()

    version = json.loads(urllib.request.urlopen(args.endpoint + "/json/version", timeout=10).read())
    print("browser:", version["Browser"])

    async with websockets.connect(version["webSocketDebuggerUrl"],
                                  max_size=64 * 1024 * 1024) as ws:
        c = Client(ws)
        if args.ip_only:
            target = (await c.call("Target.createTarget", {"url": "about:blank"}))["result"]["targetId"]
            session = (await c.call("Target.attachToTarget",
                                    {"targetId": target, "flatten": True}))["result"]["sessionId"]
            await c.call("Runtime.enable", {}, session)
            await fetch_egress_ip(c)
            print(json.dumps(await c.evaluate(
                "JSON.stringify(navigator.userAgent + ' | tz=' + "
                "Intl.DateTimeFormat().resolvedOptions().timeZone + ' | langs=' + "
                "navigator.languages.join(',') + ' | webdriver=' + navigator.webdriver)",
                session), ensure_ascii=False, indent=2))
            await c.call("Target.closeTarget", {"targetId": target})
            c._reader.cancel()
            return

        target = (await c.call("Target.createTarget", {"url": "about:blank"}))["result"]["targetId"]
        session = (await c.call("Target.attachToTarget",
                                {"targetId": target, "flatten": True}))["result"]["sessionId"]
        for domain in ("Page", "Network", "Runtime", "Log"):
            await c.call(domain + ".enable", {}, session)
        await c.call("Network.setCacheDisabled", {"cacheDisabled": True}, session)
        if not args.all_assets:
            await c.call("Network.setBlockedURLs", {"urls": BLOCKED_PATTERNS}, session)

        print(f"\nnavigate -> {args.url}")
        await c.call("Page.navigate", {"url": args.url}, session)

        for step in range(args.wait):
            await asyncio.sleep(1)
            state = await c.evaluate("document.readyState", session, timeout=5)
            if state == "complete":
                print(f"  readyState=complete after {step + 1}s")
                break
            if step % 5 == 4:
                print(f"  waiting... {step + 1}s readyState={state}")
        else:
            print(f"  still not complete after {args.wait}s")
        await asyncio.sleep(3)

        print("\n=== events ===")
        show_events(c.events)

        # Non-2xx or challenge-looking responses, with request/response details.
        api = [(p["requestId"], p["response"]) for e in c.events
               if e["method"] == "Network.responseReceived"
               for p in [e["params"]]
               if p["response"]["status"] >= 400]
        if api:
            print("\n=== rejected responses ===")
            for rid, r in api[:6]:
                print(f'  {r["status"]} {r["url"][:120]}')
                interesting = {k: v for k, v in r["headers"].items()
                               if k.lower() in ("server", "x-datadome", "set-cookie", "cf-ray",
                                                "x-amzn-waf-action", "x-amzn-requestid",
                                                "content-type", "x-cache")}
                if interesting:
                    print("    headers:", json.dumps(interesting, ensure_ascii=False)[:600])
                body = await c.call("Network.getResponseBody", {"requestId": rid}, session, timeout=8)
                raw = body.get("result", {}).get("body")
                if raw:
                    if body["result"].get("base64Encoded"):
                        import base64
                        raw = base64.b64decode(raw).decode("utf-8", "replace")
                    print("    body:", raw.replace("\n", " ")[:600])

        print("\n=== challenge frames ===")
        await dump_challenge_frames(c)

        snap = await c.evaluate(SNAPSHOT_JS, session)
        print("\n=== page ===")
        print(json.dumps(snap, ensure_ascii=False, indent=2)[:6000])

        cookies = (await c.call("Network.getCookies", {"urls": [args.url]}, session))["result"]
        print("\n=== cookies ===")
        for ck in cookies.get("cookies", []):
            flags = "".join(k[0] for k in ("httpOnly", "secure") if ck.get(k))
            print(f'  {ck["name"]:24} {ck["value"][:60]:60} {ck["domain"]} {flags}')

        if not args.no_ip:
            await fetch_egress_ip(c)

        if not args.keep:
            await c.call("Target.closeTarget", {"targetId": target})
        c._reader.cancel()


if __name__ == "__main__":
    asyncio.run(main())
