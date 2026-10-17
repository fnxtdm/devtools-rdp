"""Audit a CDP endpoint for the fingerprint tells a bot wall (DataDome) checks.

Usage:
    python3 examples/cdp_audit.py http://127.0.0.1:63943
    python3 examples/cdp_audit.py http://127.0.0.1:9222 http://127.0.0.1:63943

For each endpoint it compares what the page sees against what the browser
really is, then fires one cached and one dynamic request to show whether the
edge lets it through.
"""
import argparse
import asyncio
import json
import sys
import time
import urllib.request
from urllib.parse import urlsplit

import websockets

DEFAULT_URL = (
    "https://www.traveloka.com/en-en/flight/fullsearch"
    "?ap=CGK.DPS&dt=30-10-2026.NA&ps=1.0.0&sc=ECONOMY"
)
GREASE_HINT = "Not"

JS = r"""(async () => {
  const gl = document.createElement('canvas').getContext('webgl');
  const dbg = gl && gl.getExtension('WEBGL_debug_renderer_info');
  const o = {};
  if (navigator.userAgentData) {
    o.brands = navigator.userAgentData.brands;
    try {
      const v = await navigator.userAgentData.getHighEntropyValues(
        ['platform', 'platformVersion', 'architecture', 'fullVersionList']);
      o.hev = v;
    } catch (e) { o.hevError = String(e); }
  }
  o.cores = navigator.hardwareConcurrency;
  o.memory = navigator.deviceMemory === undefined ? null : navigator.deviceMemory;
  o.chromeApp = !!(window.chrome && window.chrome.app);
  o.webdriver = navigator.webdriver;
  o.screen = [screen.width, screen.height, screen.availWidth, screen.availHeight];
  o.window = [innerWidth, innerHeight, outerWidth, outerHeight, screenX, screenY];
  o.dpr = devicePixelRatio;
  o.glRenderer = dbg ? gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL) : null;
  o.glMaxTexture = gl ? gl.getParameter(gl.MAX_TEXTURE_SIZE) : null;
  o.glMaxUniforms = gl ? gl.getParameter(gl.MAX_VERTEX_UNIFORM_VECTORS) : null;
  return JSON.stringify(o);
})()"""


class Client:
    def __init__(self, ws):
        self.ws = ws
        self._id = 0
        self._pending = {}
        self._reader = asyncio.create_task(self._read())

    async def _read(self):
        async for raw in self.ws:
            msg = json.loads(raw)
            if "id" in msg:
                fut = self._pending.pop(msg["id"], None)
                if fut and not fut.done():
                    fut.set_result(msg)

    async def call(self, method, params=None, session=None, timeout=40):
        self._id += 1
        msg = {"id": self._id, "method": method, "params": params or {}}
        if session:
            msg["sessionId"] = session
        fut = asyncio.get_running_loop().create_future()
        self._pending[self._id] = fut
        await self.ws.send(json.dumps(msg))
        try:
            return await asyncio.wait_for(fut, timeout)
        except asyncio.TimeoutError:
            print(f"    !! {method} timed out")
            return {"result": {}}

    async def evaluate(self, expression, session, timeout=45):
        reply = await self.call("Runtime.evaluate",
                                {"expression": expression, "returnByValue": True,
                                 "awaitPromise": True}, session, timeout)
        return reply.get("result", {}).get("result", {}).get("value")


def check_ua_ch(real_version, data):
    out = []
    brands = data.get("brands") or []
    names = [b.get("brand", "") for b in brands]
    if not brands:
        out.append(("FAIL", "navigator.userAgentData 不存在"))
        return out
    if names[0] == "Google Chrome":
        out.append(("ok", f"brands 首位 {names[0]}"))
    else:
        out.append(("FAIL", f"brands 首位是 {names[0]!r}，真 Chrome 恒为 'Google Chrome'（实际顺序 {names}）"))

    grease = [n for n in names if GREASE_HINT in n]
    out.append(("info", f"GREASE 串 {grease}（真实 Chrome 同一大版本的串是固定的，跨窗口不一致即为注入）"))

    hev = data.get("hev")
    if data.get("hevError"):
        out.append(("FAIL", f"getHighEntropyValues 抛错：{data['hevError'][:70]}"))
    elif not hev:
        out.append(("FAIL", "getHighEntropyValues 无返回"))
    else:
        fvl = {b["brand"]: b["version"] for b in hev.get("fullVersionList", [])}
        claimed = fvl.get("Google Chrome") or fvl.get("Chromium")
        if claimed and real_version and claimed != real_version:
            out.append(("FAIL", f"fullVersionList 报 {claimed}，二进制实际 {real_version}"))
        elif claimed:
            out.append(("ok", f"fullVersionList {claimed} 与二进制一致"))
    return out


def check_geometry(data):
    out = []
    sw, sh, aw, ah = data["screen"]
    iw, ih, ow, oh, sx, sy = data["window"]
    rules = [
        (iw <= ow, f"innerWidth {iw} <= outerWidth {ow}"),
        (ih <= oh, f"innerHeight {ih} <= outerHeight {oh}"),
        (ow <= sw, f"outerWidth {ow} <= screen.width {sw}"),
        (oh <= sh, f"outerHeight {oh} <= screen.height {sh}"),
        (aw <= sw and ah <= sh, f"avail {aw}x{ah} <= screen {sw}x{sh}"),
        # outer sizes include invisible resize borders, so allow a small margin;
        # a maximized window on Windows reports -8,-8
        (sx >= -16 and sy >= -16 and sx + ow <= sw + 16 and sy + oh <= sh + 16,
         f"窗口 {sx},{sy} -> {sx + ow},{sy + oh} 落在屏幕内"),
    ]
    for ok, text in rules:
        out.append(("ok" if ok else "FAIL", text))
    return out


def check_hardware(data):
    out = []
    mem = data.get("memory")
    out.append(("info", f"hardwareConcurrency={data.get('cores')} deviceMemory={mem}"))
    if data.get("cores", 0) > 32:
        out.append(("FAIL", "核心数超过 32，不像真实桌面机"))
    if mem is None:
        out.append(("FAIL", "navigator.deviceMemory 缺失（真 Chrome 一定有）"))
    renderer = data.get("glRenderer") or ""
    if ("Intel" in renderer or "NVIDIA" in renderer or "AMD" in renderer) and data.get("glMaxTexture", 0) < 16384:
        out.append(("FAIL", f"渲染器声称 {renderer[:40]}，但 MAX_TEXTURE_SIZE={data.get('glMaxTexture')}，真独显/核显是 16384"))
    if data.get("webdriver"):
        out.append(("FAIL", "navigator.webdriver = true"))
    if not data.get("chromeApp"):
        out.append(("info", "window.chrome.app 缺失（真 Chrome 有，可能是注入副作用）"))
    return out


SAMPLE_JS = r"""JSON.stringify({
  host: location.host,
  t: Math.round(performance.now()),
  uaData: typeof navigator.userAgentData,
  brand0: navigator.userAgentData ? navigator.userAgentData.brands[0].brand : null,
  mem: navigator.deviceMemory === undefined ? null : navigator.deviceMemory,
  app: !!(window.chrome && window.chrome.app)
})"""


async def check_injection(c, origin):
    """A real browser has the same navigator from the first millisecond; a
    patched one can differ between an early and a late sample of one document."""
    target = (await c.call("Target.createTarget", {"url": "about:blank"}))["result"].get("targetId")
    if not target:
        return [("info", "无法创建标签页")]
    session = (await c.call("Target.attachToTarget",
                            {"targetId": target, "flatten": True}))["result"].get("sessionId")
    await c.call("Runtime.enable", {}, session)
    await c.call("Page.navigate", {"url": origin + "/robots.txt"}, session, timeout=20)
    early = await c.evaluate(SAMPLE_JS, session, timeout=20)
    await asyncio.sleep(1.5)
    late = await c.evaluate(SAMPLE_JS, session, timeout=20)
    await c.call("Target.closeTarget", {"targetId": target})

    try:
        a, b = json.loads(early), json.loads(late)
    except (TypeError, ValueError):
        return [("info", "采样失败，跳过时序检查")]
    if a.get("host") != b.get("host"):
        return [("info", "两次采样落在不同文档，跳过")]
    if a.get("host") == "":
        return [("info", "首次采样仍在 about:blank，跳过")]
    keys = ("uaData", "brand0", "mem", "app")
    if all(a.get(k) == b.get(k) for k in keys):
        return [("info", f"两次采样一致（{a.get('host')}，t={a.get('t')}ms）；改写可能已在 document_start 完成，"
                         "该窗口未捕获到时间差")]
    diff = ", ".join(f"{k}: {a.get(k)!r} -> {b.get(k)!r}" for k in keys if a.get(k) != b.get(k))
    return [("FAIL", f"导航后 navigator 才被改写（同一文档内取值变化）：{diff}")]


async def audit(endpoint, url):
    print(f"\n=== {endpoint}")
    version = json.loads(urllib.request.urlopen(endpoint + "/json/version", timeout=15).read())
    browser = version["Browser"]
    real_version = browser.split("/", 1)[1] if "/" in browser else ""
    print(f"    {browser}   (二进制版本 {real_version})")

    async with websockets.connect(version["webSocketDebuggerUrl"], max_size=64 * 1024 * 1024,
                                  open_timeout=45, ping_interval=None) as ws:
        c = Client(ws)
        origin = "{0.scheme}://{0.netloc}".format(urlsplit(url))
        target = (await c.call("Target.createTarget", {"url": origin + "/robots.txt"}))["result"].get("targetId")
        if not target:
            print("    !! 无法创建标签页")
            c._reader.cancel()
            return
        session = (await c.call("Target.attachToTarget",
                                {"targetId": target, "flatten": True}))["result"]["sessionId"]
        await asyncio.sleep(2)

        raw = await c.evaluate(JS, session)
        data = json.loads(raw) if raw else {}

        print("\n  -- Client Hints --")
        for level, text in check_ua_ch(real_version, data):
            print(f"    [{level}] {text}")
        print("\n  -- 窗口几何 --")
        for level, text in check_geometry(data):
            print(f"    [{level}] {text}")
        print("\n  -- 硬件/自动化 --")
        for level, text in check_hardware(data):
            print(f"    [{level}] {text}")

        print("\n  -- 注入时序 --")
        for level, text in await check_injection(c, origin):
            print(f"    [{level}] {text}")

        probe = r"""(async () => {
          const get = async (p) => {
            const r = await fetch(p, {headers: {'accept': 'text/html'}});
            const t = await r.text();
            return [r.status, r.headers.get('x-datadome'), r.headers.get('x-cache'), t.length];
          };
          return JSON.stringify({
            static: await get('/'),
            dynamic: await get(__URL__),
          });
        })()""".replace("__URL__", json.dumps(urlsplit(url).path + "?" + urlsplit(url).query + "&audit=" + str(int(time.time()))))
        raw = await c.evaluate(probe, session, timeout=60)
        print("\n  -- 边缘放行情况 --")
        res = json.loads(raw) if isinstance(raw, (str, bytes, bytearray)) else None
        if res:
            for kind in ("static", "dynamic"):
                status, dd, cache, length = res[kind]
                mark = "ok" if status < 400 else "FAIL"
                print(f"    [{mark}] {kind:7} {status}  x-datadome={dd}  cache={cache}  {length}B")
        else:
            print(f"    (探测失败，页面返回 {type(raw).__name__}: {str(raw)[:80]!r})")

        print("\n  -- 这台机器的出口 --")
        await c.call("Page.navigate", {"url": "https://ipinfo.io/json"}, session)
        body = None
        for _ in range(20):
            await asyncio.sleep(0.5)
            body = await c.evaluate("document.body ? document.body.innerText : ''", session, timeout=10)
            if body and body.strip().startswith("{"):
                break
        try:
            info = json.loads(body)
            print(f"    ip={info.get('ip')}  org={info.get('org')}  "
                  f"{info.get('city')}/{info.get('country')}  tz={info.get('timezone')}")
        except (TypeError, ValueError):
            print(f"    (ipinfo 无返回: {str(body)[:80]!r})")

        await c.call("Target.closeTarget", {"targetId": target})
        c._reader.cancel()


async def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("endpoints", nargs="+")
    ap.add_argument("--url", default=DEFAULT_URL)
    args = ap.parse_args()
    for ep in args.endpoints:
        try:
            await audit(ep, args.url)
        except Exception as exc:
            print(f"\n=== {ep}\n    !! {exc.__class__.__name__}: {exc}")


if __name__ == "__main__":
    asyncio.run(main())
