#!/usr/bin/env python3
"""Minimal Mono client in Python, using only the standard library.

It talks to the player HTTP gateway (rts-gateway, default :8080), which
exposes GameService as protojson over HTTP with NDJSON streams:

  POST /api/connect   {"joinToken": ...}            -> ConnectResponse
  POST /api/command   SubmitCommandRequest          -> SubmitCommandResponse
  GET  /api/snapshot                                -> GetSnapshotResponse
  GET  /api/observe   one ObserveResponse per line   (snapshot, then updates)

Strategy: every 20 ticks, send one idle unit to a random tile.

    python3 examples/python/bot.py --url http://127.0.0.1:8080 --token red --ticks 100
"""
import argparse
import json
import random
import urllib.request

MOVE = "type.googleapis.com/rts.movement.v1.GroupMoveCommand"
READY = "type.googleapis.com/rts.match.v1.SetReadyCommand"


class Client:
    def __init__(self, url, token):
        self.url = url.rstrip("/")
        resp = self._call("POST", "/api/connect", {"joinToken": token}, auth=False)
        self.session = resp["sessionToken"]
        self.player = resp["playerId"]
        self.world = resp["world"]
        self.seq = 0
        # Command IDs must be unique per player: the server answers a
        # repeated ID with the original acknowledgment and does not run it
        # again. A per-run nonce keeps restarts from colliding.
        self.nonce = "%08x" % random.getrandbits(32)

    def _request(self, method, path, body=None, auth=True):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.url + path, data=data, method=method)
        req.add_header("Content-Type", "application/json")
        if auth:
            req.add_header("Authorization", "Bearer " + self.session)
        return urllib.request.urlopen(req)

    def _call(self, method, path, body=None, auth=True):
        with self._request(method, path, body, auth) as r:
            return json.load(r)

    def submit(self, payload):
        """Schedules a command. Acceptance is not execution: the result
        arrives later in an update's commandResults."""
        self.seq += 1
        cid = f"py-{self.nonce}-{self.seq}"
        ack = self._call("POST", "/api/command", {"commandId": cid, "payload": payload})
        if not ack["accepted"]:
            print("rejected:", ack["rejectionReason"])
        return cid

    def observe(self):
        """Yields ObserveResponse objects: a snapshot first, then updates."""
        with self._request("GET", "/api/observe") as r:
            for line in r:
                if line.strip():
                    yield json.loads(line)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://127.0.0.1:8080")
    ap.add_argument("--token", default="red")
    ap.add_argument("--ticks", type=int, default=100)
    args = ap.parse_args()

    c = Client(args.url, args.token)
    w = c.world
    print(f"player {c.player}: {w['width']}x{w['height']} at {w['tickRateHz']} Hz, budget {w['unitCommandBudget']}")
    entities = {}  # id -> entity: our client-side view
    match = {}     # MatchInfo: lifecycle, readiness, clock, result
    ticks = 0
    for msg in c.observe():
        m = (msg.get("snapshot") or msg.get("update") or {}).get("match")
        if m:
            if match and m["matchId"] != match["matchId"]:
                print("new match (reset):", m["matchId"])
            if m.get("yourPlayerId"):
                c.player = m["yourPlayerId"]  # player IDs are per match
            if m.get("result") and not match.get("result"):
                r = m["result"]
                print(f"match over: winner {r['winnerId']} draw {r['draw']} reason {r['reason']}")
            match = m
        if "snapshot" in msg:
            snap = msg["snapshot"]
            entities = {e["id"]: e for e in snap.get("entities", [])}
            print(f"snapshot tick {snap['tick']}: {len(entities)} entities, fog={snap.get('fogOfWar')}")
            continue
        u = msg["update"]
        for e in u.get("changed", []):  # full replacements
            entities[e["id"]] = e
        for eid in u.get("removed", []):
            entities.pop(eid, None)
        for r in u.get("commandResults", []):
            print(f"tick {u['tick']}: {r['commandId']} -> {r['status']} {r.get('errorCode', '')}")
        ticks += 1
        tick = int(u["tick"])
        state = match.get("state", "")
        if state == "MATCH_STATE_READY" and not match.get("youReady") and tick % 10 == 0:
            c.submit({"@type": READY, "ready": True})
            print(f"tick {tick}: ready")
        if state == "MATCH_STATE_RUNNING" and tick % 20 == 0:
            idle = [e for e in entities.values()
                    if e.get("ownerId") == c.player and e["kind"] == "ENTITY_KIND_UNIT"
                    and "movement.destination" not in e.get("attributes", {})]
            if idle:
                unit = random.choice(idle)
                dest = {"x": random.randrange(w["width"]), "y": random.randrange(w["height"])}
                cid = c.submit({"@type": MOVE, "unitIds": [unit["id"]], "destination": dest})
                print(f"tick {tick}: {cid} move {unit['id']} -> ({dest['x']},{dest['y']})")
        if ticks >= args.ticks:
            mine = sum(1 for e in entities.values() if e.get("ownerId") == c.player)
            print(f"done: {mine} own entities, {len(entities)} in view")
            return


if __name__ == "__main__":
    main()
