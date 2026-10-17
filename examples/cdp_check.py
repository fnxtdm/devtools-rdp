"""Smoke test: open a target, evaluate JS via the devtools-rdp tunnel."""
import asyncio
import json
import urllib.request

import websockets

ENDPOINT = "http://127.0.0.1:9222"


async def main():
    version = json.loads(urllib.request.urlopen(ENDPOINT + "/json/version", timeout=10).read())
    print("browser:", version["Browser"])
    async with websockets.connect(version["webSocketDebuggerUrl"], max_size=64 * 1024 * 1024) as ws:
        counter = 0

        async def call(method, params=None, session_id=None):
            nonlocal counter
            counter += 1
            msg = {"id": counter, "method": method, "params": params or {}}
            if session_id:
                msg["sessionId"] = session_id
            await ws.send(json.dumps(msg))
            while True:
                reply = json.loads(await ws.recv())
                if reply.get("id") == counter:
                    return reply["result"]

        target = (await call("Target.createTarget", {"url": "about:blank"}))["targetId"]
        session = (await call("Target.attachToTarget", {"targetId": target, "flatten": True}))["sessionId"]
        value = (await call("Runtime.evaluate", {"expression": "2 + 40", "returnByValue": True}, session))["result"]["value"]
        print("Runtime.evaluate(2 + 40) =", value)
        await call("Target.closeTarget", {"targetId": target})
        assert value == 42


if __name__ == "__main__":
    asyncio.run(main())
