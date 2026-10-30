#!/usr/bin/env python3
"""minivpn watchdog: ends the session from the node's side.

Runs every minute. Reads how much traffic went through sing-box since the last
run, keeps a short history, and deletes this server through the hoster API
when the node has been idle long enough or is simply too old. This covers
the case the app cannot: the PC fell asleep, crashed or lost the network.

Standard library only: the node has nothing else installed.
"""
import json
import os
import sys
import time
import urllib.request

CONF = os.environ.get("MINIVPN_WATCHDOG_CONF", "/usr/local/etc/minivpn/watchdog.json")
STATE = os.environ.get("MINIVPN_WATCHDOG_STATE", "/var/lib/minivpn/traffic.json")
BORN = "/var/lib/minivpn/born"  # unix time the setup script started
TOTAL = STATE + ".total"  # sing-box's byte counter at the previous run


def is_idle(samples, now, idle_s, window_s, limit):
    """Every window_s slice of the last idle_s carried less than limit bytes.

    samples is a list of [unix_time, bytes_since_previous_sample]. The caller
    makes sure the node is at least idle_s old; a slice with no samples at
    all counts as idle (the watchdog missed ticks, nothing went through).
    """
    start = now - idle_s
    for i in range(int(idle_s // window_s)):
        lo = start + i * window_s
        hi = lo + window_s
        total = sum(b for t, b in samples if lo < t <= hi)
        if total >= limit:
            return False
    return True


def since(total, last):
    """Bytes since the previous run, from sing-box's running totals.

    A total below the last one means sing-box restarted and counts from zero.
    """
    if last is None or total < last:
        return total
    return total - last


def traffic(conf):
    """Bytes through the node since the previous call."""
    with urllib.request.urlopen(conf["stats_url"], timeout=20) as resp:
        stats = json.load(resp)
    total = int(stats.get("downloadTotal", 0)) + int(stats.get("uploadTotal", 0))
    try:
        with open(TOTAL) as f:
            last = int(f.read())
    except (OSError, ValueError):
        last = None
    with open(TOTAL, "w") as f:
        f.write(str(total))
    return since(total, last)


def age(now):
    """Seconds since setup. Falls back to uptime if the mark is missing."""
    try:
        with open(BORN) as f:
            return now - float(f.read())
    except (OSError, ValueError):
        with open("/proc/uptime") as f:
            return float(f.read().split()[0])


def api(conf, method, path):
    req = urllib.request.Request(
        conf["api_base"] + path, method=method,
        headers={"X-API-KEY": conf["api_key"], "Accept": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        body = resp.read()
    return json.loads(body) if body else {}


def own_servers(conf):
    servers = api(conf, "GET", "/api/v1/servers").get("servers") or []
    return [s for s in servers if s.get("name") == conf["name"]]


def delete_self(conf, reason):
    mine = own_servers(conf)
    if not mine:
        print("watchdog: server %s not found in the API, nothing to delete" % conf["name"])
        return
    for s in mine:
        print("watchdog: deleting %s (%s): %s" % (s["id"], conf["name"], reason))
        api(conf, "DELETE", "/api/v1/servers/" + s["id"])


def main():
    with open(CONF) as f:
        conf = json.load(f)
    now = time.time()
    try:
        with open(STATE) as f:
            samples = json.load(f)
    except (OSError, ValueError):
        samples = []

    try:
        got = traffic(conf)
    except Exception as e:  # sing-box not up yet or broken: count it as no traffic.
        print("watchdog: stats unavailable: %s" % e)
        got = 0
    samples.append([now, got])
    keep_from = now - conf["idle_seconds"] - conf["window_seconds"]
    samples = [s for s in samples if s[0] >= keep_from]
    with open(STATE + ".tmp", "w") as f:
        json.dump(samples, f)
    os.replace(STATE + ".tmp", STATE)

    node_age = age(now)
    if node_age >= conf["max_seconds"]:
        reason = "max age %ds" % conf["max_seconds"]
    elif node_age >= conf["idle_seconds"] and is_idle(
        samples, now, conf["idle_seconds"], conf["window_seconds"], conf["idle_bytes"]
    ):
        reason = "idle for %ds" % conf["idle_seconds"]
    else:
        return 0
    delete_self(conf, reason)
    return 0


if __name__ == "__main__":
    sys.exit(main())
