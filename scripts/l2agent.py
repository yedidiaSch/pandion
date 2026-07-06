#!/usr/bin/env python3
# Pandion L2 conformance/stress agent — runs on each node, no dependencies
# (Python 3 stdlib + raw AF_PACKET). It exercises the Layer-2 overlay directly at
# the Ethernet level so it can prove the properties an IP-only test cannot:
#   * unicast full-mesh delivery (every ordered node pair)
#   * broadcast fan-out (BUM head-end replication reaches ALL peers)
#   * multicast fan-out
#   * L2 transparency (arbitrary non-IP EtherType frames cross intact)
#   * precise MTU boundary (frames <= inner MTU cross; larger are dropped)
#   * ARP spoof (host-side Dynamic ARP Inspection blocks forged bindings)
#
# All probe frames use a private experimental EtherType (0x88B5) with a magic
# payload "PANDION-L2|<kind>|<src_id>|<seq>|<size>", so a listener can classify
# every frame by kind (U/B/M), source, and size regardless of destination.
#
# Subcommands:
#   listen  <iface> <seconds> <outfile>
#   send    <iface> <self_id> <peers_json> [sizes_csv]
#   spoof   <iface> <dst_mac> <src_mac> <impersonate_ip> <victim_ip> <victim_mac>
import json
import os
import select
import socket
import struct
import sys
import time

ETH_P_ALL = 0x0003
ETHERTYPE = 0x88B5  # IEEE 802 local experimental (non-IP, non-ARP)
MAGIC = b"PANDION-L2"
BCAST = "ff:ff:ff:ff:ff:ff"
MCAST = "01:00:5e:00:00:7b"  # an L2 multicast group address
DEFAULT_SIZES = [64, 512, 1300, 1370, 1372, 1450]  # payload bytes (MTU boundary sweep)


def mac_bytes(m):
    return bytes(int(x, 16) for x in m.split(":"))


def mac_str(b):
    return ":".join("%02x" % x for x in b)


def build(dst, src, kind, src_id, seq, size):
    body = b"|".join([MAGIC, kind.encode(), src_id.encode(), str(seq).encode(), str(size).encode()])
    if len(body) < size:
        body = body + b"\x00" * (size - len(body))
    return mac_bytes(dst) + mac_bytes(src) + struct.pack("!H", ETHERTYPE) + body[:max(size, len(body))]


def self_mac(iface):
    s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW)
    s.bind((iface, 0))
    m = s.getsockname()
    s.close()
    # getsockname doesn't give MAC portably; read from sysfs.
    with open("/sys/class/net/%s/address" % iface) as f:
        return f.read().strip()


def do_listen(iface, seconds, outfile):
    s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(ETH_P_ALL))
    s.bind((iface, 0))
    s.setblocking(False)
    end = time.time() + float(seconds)
    seen = []  # (kind, src_id, seq, size, observed_src_mac)
    while time.time() < end:
        r, _, _ = select.select([s], [], [], 0.25)
        if not r:
            continue
        try:
            frame = s.recv(65535)
        except BlockingIOError:
            continue
        if len(frame) < 14:
            continue
        etype = struct.unpack("!H", frame[12:14])[0]
        if etype != ETHERTYPE:
            continue
        payload = frame[14:]
        if not payload.startswith(MAGIC):
            continue
        parts = payload.split(b"|")
        if len(parts) < 5:
            continue
        kind, sid, seq, size = parts[1].decode(), parts[2].decode(), parts[3].decode(), parts[4].split(b"\x00")[0].decode()
        seen.append((kind, sid, seq, size, mac_str(frame[6:12])))
    with open(outfile, "w") as f:
        for k, sid, seq, size, srcmac in seen:
            f.write("%s %s %s %s %s\n" % (k, sid, seq, size, srcmac))
    print("listened %ss, captured %d probes" % (seconds, len(seen)))


def do_send(iface, self_id, peers_json, sizes_csv=None):
    peers = json.loads(peers_json)  # [{"id":..,"mac":..}, ...]
    sizes = [int(x) for x in sizes_csv.split(",")] if sizes_csv else DEFAULT_SIZES
    src = self_mac(iface)
    s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW)
    s.bind((iface, 0))
    sent = 0

    def emit(dst, kind, size):
        nonlocal sent
        try:
            s.send(build(dst, src, kind, self_id, sent, size))
            sent += 1
        except OSError:
            pass  # e.g. frame > MTU is refused locally — that's the boundary we test

    # unicast to each peer, across the size sweep (MTU boundary per pair).
    for p in peers:
        for sz in sizes:
            emit(p["mac"], "U", sz)
    # one broadcast + one multicast per size class (fan-out to ALL peers via BUM).
    for sz in (64, 1300):
        emit(BCAST, "B", sz)
        emit(MCAST, "M", sz)
    print("sent %d probes from %s" % (sent, self_id))


def do_spoof(iface, dst_mac, src_mac, imp_ip, vic_ip, vic_mac):
    s = socket.socket(socket.AF_PACKET, socket.SOCK_RAW)
    s.bind((iface, 0))
    eth = mac_bytes(dst_mac) + mac_bytes(src_mac) + struct.pack("!H", 0x0806)
    arp = struct.pack("!HHBBH", 1, 0x0800, 6, 4, 2)  # reply
    arp += mac_bytes(src_mac) + socket.inet_aton(imp_ip)  # sender = attacker mac, impersonated ip
    arp += mac_bytes(vic_mac) + socket.inet_aton(vic_ip)  # target = victim
    for _ in range(8):
        s.send(eth + arp)
    print("sent 8 forged ARP replies: %s is-at %s -> %s" % (imp_ip, src_mac, vic_ip))


def main():
    if len(sys.argv) < 2:
        print("usage: l2agent.py listen|send|spoof ...", file=sys.stderr)
        sys.exit(2)
    cmd = sys.argv[1]
    if cmd == "listen":
        do_listen(sys.argv[2], sys.argv[3], sys.argv[4])
    elif cmd == "send":
        do_send(sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5] if len(sys.argv) > 5 else None)
    elif cmd == "spoof":
        do_spoof(*sys.argv[2:8])
    else:
        print("unknown subcommand %r" % cmd, file=sys.stderr)
        sys.exit(2)


if __name__ == "__main__":
    main()
