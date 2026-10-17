"""Compare the browser fingerprint exposed on several CDP endpoints.

Usage:
    python3 examples/fp_compare.py http://127.0.0.1:9222 http://127.0.0.1:51110

Useful when one browser passes a bot wall and another does not: the rows marked
"<-" differ between endpoints, which is where an anti-detect browser usually
leaves a tell.
"""
import asyncio
import json
import sys
import urllib.request

import websockets

# UA-CH (navigator.userAgentData) only resolves on a secure context, so probe a
# real https page instead of about:blank.
PROBE_URL = "https://example.com"

JS = r"""(async () => {
  const nav = navigator;
  const gl = document.createElement('canvas').getContext('webgl');
  const dbg = gl && gl.getExtension('WEBGL_debug_renderer_info');
  const nativeOf = f => { try { return Function.prototype.toString.call(f).includes('[native code]'); } catch (e) { return 'err'; } };
  let ch = null;
  try { ch = nav.userAgentData ? await nav.userAgentData.getHighEntropyValues(
      ['platformVersion','architecture','bitness','model','uaFullVersion','fullVersionList']) : null; } catch (e) {}
  const c = document.createElement('canvas');
  c.width = 200; c.height = 40;
  const ctx = c.getContext('2d');
  ctx.textBaseline = 'top'; ctx.font = '14px Arial';
  ctx.fillStyle = '#f60'; ctx.fillRect(0, 0, 200, 40);
  ctx.fillStyle = '#069'; ctx.fillText('devtools-rdp', 2, 6);
  let canvas = 0;
  for (const b of c.toDataURL()) canvas = (canvas * 31 + b.charCodeAt(0)) | 0;
  const ext = gl ? (gl.getSupportedExtensions() || []).sort() : [];
  const glNum = n => { try { return gl.getParameter(gl[n]); } catch (e) { return null; } };
  return JSON.stringify({
    ua: nav.userAgent,
    uaChPlatform: ch && ch.platform, uaChPlatformVer: ch && ch.platformVersion,
    uaChArch: ch && ch.architecture, uaChBitness: ch && ch.bitness,
    uaChModel: ch && ch.model, uaChMobile: ch && ch.mobile,
    uaChFull: ch && (ch.fullVersionList || []).map(b => b.brand + ' ' + b.version).join(','),
    glVersion: gl ? gl.getParameter(gl.VERSION) : null,
    glShading: gl ? gl.getParameter(gl.SHADING_LANGUAGE_VERSION) : null,
    glMaxTexture: glNum('MAX_TEXTURE_SIZE'),
    glMaxViewport: gl ? gl.getParameter(gl.MAX_VIEWPORT_DIMS).join('x') : null,
    glMaxVertexUniforms: glNum('MAX_VERTEX_UNIFORM_VECTORS'),
    glMaxRenderbuffer: glNum('MAX_RENDERBUFFER_SIZE'),
    glExtensions: ext.length + ' ' + (ext.join(',').length),
    glKeyExt: ['WEBGL_compressed_texture_s3tc', 'WEBGL_compressed_texture_etc',
      'EXT_texture_filter_anisotropic', 'WEBGL_debug_renderer_info', 'OES_texture_float_linear',
      'WEBGL_draw_buffers'].map(e => (ext.includes(e) ? 1 : 0)).join(''),
    anisoMax: (() => { try { const e = gl.getExtension('EXT_texture_filter_anisotropic');
      return e ? gl.getParameter(e.MAX_TEXTURE_MAX_ANISOTROPY_EXT) : null; } catch (x) { return null; } })(),
    precision: (() => { try { const f = gl.getShaderPrecisionFormat(gl.FRAGMENT_SHADER, gl.HIGH_FLOAT);
      return f ? f.precision + '/' + f.rangeMin + '/' + f.rangeMax : null; } catch (x) { return null; } })(),
    platform: nav.platform, vendor: nav.vendor,
    webdriver: nav.webdriver, chromeObj: !!window.chrome,
    chromeRuntime: !!(window.chrome && window.chrome.runtime),
    chromeApp: !!(window.chrome && window.chrome.app),
    chromeCsi: typeof (window.chrome && window.chrome.csi),
    plugins: nav.plugins.length, mimeTypes: nav.mimeTypes.length,
    languages: (nav.languages || []).join(','), language: nav.language,
    tz: Intl.DateTimeFormat().resolvedOptions().timeZone,
    tzOffset: new Date().getTimezoneOffset(),
    screen: [screen.width, screen.height, screen.availWidth, screen.availHeight].join('x'),
    window: [outerWidth, outerHeight, innerWidth, innerHeight].join('x'),
    geoOrigin: [screenX, screenY, Math.round(screenX + outerWidth), Math.round(screenY + outerHeight)].join(','),
    geoChrome: [outerWidth - innerWidth, outerHeight - innerHeight].join(','),
    geoOk: [
      innerWidth <= outerWidth,
      innerHeight <= outerHeight,
      outerWidth <= screen.width,
      outerHeight <= screen.height,
      screenX >= 0, screenY >= 0,
      screenX + outerWidth <= screen.width,
      screenY + outerHeight <= screen.height,
      screen.availWidth <= screen.width,
      screen.availHeight <= screen.height,
    ].every(Boolean),
    dpr: devicePixelRatio, colorDepth: screen.colorDepth,
    cores: nav.hardwareConcurrency, memory: nav.deviceMemory,
    webglVendor: dbg ? gl.getParameter(dbg.UNMASKED_VENDOR_WEBGL) : null,
    webglRenderer: dbg ? gl.getParameter(dbg.UNMASKED_RENDERER_WEBGL) : null,
    canvasHash: canvas,
    permissionsNative: nativeOf(nav.permissions.query),
    toStringNative: nativeOf(Function.prototype.toString),
    cdc: Object.keys(window).filter(k => k.startsWith('cdc_') || k.startsWith('$cdc')).join(','),
    connection: nav.connection ? nav.connection.effectiveType + '/' + nav.connection.rtt : null,
    notification: typeof Notification !== 'undefined' ? Notification.permission : null,
    maxTouch: nav.maxTouchPoints,
    perfMemory: !!(performance && performance.memory),
    pdfViewer: !!nav.pdfViewerEnabled,
  });
})()"""


async def snapshot(endpoint):
    version = json.loads(urllib.request.urlopen(endpoint + "/json/version", timeout=10).read())
    async with websockets.connect(version["webSocketDebuggerUrl"], max_size=32 * 1024 * 1024) as ws:
        counter = 0

        async def call(method, params=None, session=None, timeout=30):
            nonlocal counter
            counter += 1
            msg = {"id": counter, "method": method, "params": params or {}}
            if session:
                msg["sessionId"] = session
            await ws.send(json.dumps(msg))
            while True:
                reply = json.loads(await asyncio.wait_for(ws.recv(), timeout))
                if reply.get("id") == counter:
                    return reply

        target = (await call("Target.createTarget", {"url": PROBE_URL}))["result"]["targetId"]
        session = (await call("Target.attachToTarget",
                              {"targetId": target, "flatten": True}))["result"]["sessionId"]
        await call("Runtime.enable", {}, session)
        for _ in range(20):
            await asyncio.sleep(0.5)
            state = (await call("Runtime.evaluate",
                                {"expression": "document.readyState", "returnByValue": True},
                                session)).get("result", {}).get("result", {}).get("value")
            if state == "complete":
                break
        reply = await call("Runtime.evaluate",
                           {"expression": JS, "returnByValue": True, "awaitPromise": True}, session)
        await call("Target.closeTarget", {"targetId": target})
        return version["Browser"], json.loads(reply["result"]["result"]["value"])


async def main():
    endpoints = sys.argv[1:] or ["http://127.0.0.1:9222", "http://127.0.0.1:51110"]
    snaps = []
    for ep in endpoints:
        browser, fp = await snapshot(ep)
        snaps.append((ep, browser, fp))
        print(f"{ep}: {browser}")

    keys = list(snaps[0][2].keys())
    width = max(len(k) for k in keys)
    print("\n=== fingerprint ===")
    header = " " * (width + 2) + "  ".join(ep.split("//")[1].ljust(10) for ep in endpoints)
    print(header)
    for k in keys:
        vals = [json.dumps(s[2].get(k), ensure_ascii=False) for s in snaps]
        mark = "  <-" if len(set(vals)) > 1 else ""
        print(f"{k.ljust(width)}  " + "  ".join(v[:34].ljust(10) for v in vals) + mark)
    print("\nrows marked <- differ between browsers")


if __name__ == "__main__":
    asyncio.run(main())
