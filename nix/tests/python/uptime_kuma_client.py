"""Uptime Kuma socket.io client for the monitor-uptime-kuma VM test; prints JSON.

usage: uptime_kuma_client.py URL USER PASSWORD COMMAND [ARGS]
  setup                 create the administrator unless one exists
  login                 print a fresh session token
  token-ok TOKEN        print whether a token from an earlier login is still accepted
  add-http NAME TARGET  add an HTTP monitor polling TARGET every 20 s; print its id
  add-push PREFIX N     add N push monitors, one acknowledged at a time; print their names
  list                  print every monitor's name by id
  wait-up ID SECONDS    wait for monitor ID to report UP
"""

import json
import sys
import threading
import time

import socketio

TIMEOUT = 30


def connect(url):
    events = {"monitorList": threading.Event(), "up": set()}
    sio = socketio.Client(reconnection=False)

    @sio.on("monitorList")
    def on_list(monitors):
        events["monitors"] = {int(k): v["name"] for k, v in monitors.items()}
        events["monitorList"].set()

    @sio.on("heartbeat")
    def on_beat(beat):
        if beat.get("status") == 1:
            events["up"].add(beat["monitorID"])

    @sio.on("heartbeatList")
    def on_beats(monitor_id, beats, overwrite=False):
        if any(b.get("status") == 1 for b in beats):
            events["up"].add(int(monitor_id))

    sio.connect(url, transports=["websocket"], wait_timeout=TIMEOUT)
    return sio, events


def call(sio, event, *args):
    data = args[0] if len(args) == 1 else (args or None)
    res = sio.call(event, data, timeout=TIMEOUT)
    if isinstance(res, dict) and not res.get("ok", False):
        raise SystemExit(f"{event} failed: {res}")
    return res


def login(sio, user, password):
    return call(sio, "login", {"username": user, "password": password, "token": ""})["token"]


def monitor(kind, name, **extra):
    return {"type": kind, "name": name, "interval": 20, "retryInterval": 20, "maxretries": 0,
            "accepted_statuscodes": ["200-299"], "notificationIDList": {}, "kafkaProducerBrokers": [],
            "kafkaProducerSaslOptions": {"mechanism": "None"}, "conditions": [], "rabbitmqNodes": [], **extra}


def main(url, user, password, command, *args):
    sio, events = connect(url)
    try:
        if command == "setup":
            if sio.call("needSetup", timeout=TIMEOUT):
                call(sio, "setup", user, password)
            return True
        if command == "token-ok":
            return sio.call("loginByToken", args[0], timeout=TIMEOUT).get("ok", False)
        token = login(sio, user, password)
        if command == "login":
            return token
        if command == "add-http":
            return call(sio, "add", monitor("http", args[0], url=args[1]))["monitorID"]
        if command == "add-push":
            names = [f"{args[0]}{n}" for n in range(int(args[1]))]
            for n, name in enumerate(names):
                call(sio, "add", monitor("push", name, pushToken=f"push{n:026d}"))
            return names
        if command == "list":
            events["monitorList"].clear()
            call(sio, "getMonitorList")
            if not events["monitorList"].wait(TIMEOUT):
                raise SystemExit("no monitorList after getMonitorList")
            return events["monitors"]
        if command == "wait-up":
            deadline = time.time() + int(args[1])
            while int(args[0]) not in events["up"]:
                if time.time() > deadline:
                    raise SystemExit(f"monitor {args[0]} not UP in {args[1]}s")
                time.sleep(1)
            return True
        raise SystemExit(f"unknown command {command}")
    finally:
        sio.disconnect()


if __name__ == "__main__":
    print(json.dumps(main(*sys.argv[1:])))
