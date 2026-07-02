#!/usr/bin/env python3
"""Disposable stdlib-only proof for the agentchute conformance vectors.

This is intentionally small and not wired into CI. The vectors are the durable
contract; this file only proves they can drive a non-Go implementation.
"""

from __future__ import annotations

import json
import pathlib
import sys
import threading
import time
from dataclasses import dataclass, field


ROOT = pathlib.Path(__file__).resolve().parents[1]
VECTORS = ROOT / "vectors" / "core.json"


@dataclass
class Msg:
    from_id: str = ""
    body: str = ""
    reply_required: bool = False
    in_reply_to: str = ""
    key: str = ""
    extra: dict[str, str] = field(default_factory=dict)
    seq: int = 0


class InboxBinding:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._inbox: dict[str, list[Msg]] = {}
        self._seen: dict[str, float] = {}
        self._delivered: set[tuple[str, str, int]] = set()
        self._crash_after_act = False
        self.window = 30.0

    def register(self, agent: str) -> None:
        with self._lock:
            self._inbox.setdefault(agent, [])
            self._seen[agent] = time.time()

    def presence(self, agent: str) -> tuple[bool, bool]:
        with self._lock:
            if agent not in self._seen:
                return False, False
            return time.time() - self._seen[agent] < self.window, True

    def force_last_seen(self, agent: str, seconds_ago: int) -> None:
        with self._lock:
            self._seen[agent] = time.time() - seconds_ago

    def deliver(self, to: str, msg: Msg) -> None:
        if not msg.from_id:
            raise ValueError("E1: message has no from")
        with self._lock:
            if to not in self._inbox:
                raise ValueError(f"unknown recipient {to!r}")
            if msg.seq:
                key = (to, msg.from_id, msg.seq)
                if key in self._delivered:
                    return
                self._delivered.add(key)
            self._inbox[to].append(msg)

    def stage_delivery(self, to: str, msg: Msg) -> callable:
        if not msg.from_id:
            raise ValueError("E1: message has no from")
        with self._lock:
            if to not in self._inbox:
                raise ValueError(f"unknown recipient {to!r}")

        def commit() -> None:
            self.deliver(to, msg)

        return commit

    def poll(self, agent: str) -> list[Msg]:
        with self._lock:
            return list(self._inbox.get(agent, []))

    def crash_after_act_once(self) -> None:
        self._crash_after_act = True

    def consume(self, agent: str, handler: callable) -> int:
        count = 0
        for msg in self.poll(agent):
            handler(msg)
            if self._crash_after_act:
                self._crash_after_act = False
                raise RuntimeError("simulated crash after act")
            with self._lock:
                if self._inbox.get(agent):
                    removed = self._inbox[agent].pop(0)
                    if removed.seq:
                        self._delivered.discard((agent, removed.from_id, removed.seq))
                self._seen[agent] = time.time()
            count += 1
        return count

    def private_bodies(self) -> bool:
        return True

    def peek_bodies(self, owner: str, reader: str) -> list[str]:
        if owner != reader:
            return []
        return [m.body for m in self.poll(owner)]


class Deduper:
    def __init__(self) -> None:
        self.seen: set[str] = set()

    def once(self, msg: Msg, fn: callable) -> None:
        if msg.key and msg.key in self.seen:
            return
        fn(msg)
        if msg.key:
            self.seen.add(msg.key)


def msg(data: dict) -> Msg:
    return Msg(
        from_id=data.get("from", ""),
        body=data.get("body", ""),
        reply_required=data.get("reply_required", False),
        in_reply_to=data.get("in_reply_to", ""),
        key=data.get("key", ""),
        extra=data.get("extra", {}),
        seq=data.get("seq", 0),
    )


def load_vectors() -> dict[str, dict]:
    data = json.loads(VECTORS.read_text())
    if data.get("schema") != "agentchute-conformance-vectors-v1":
        raise AssertionError(f"unexpected schema {data.get('schema')!r}")
    vectors = {v["id"]: v for v in data["vectors"]}
    for required in ("R1", "D1", "D2", "O1", "C1", "E1", "B1"):
        if required not in vectors:
            raise AssertionError(f"missing vector {required}")
    return vectors


def run_r1(v: dict) -> None:
    b = InboxBinding()
    assert b.presence(v["agent"]) == (False, False)
    b.register(v["agent"])
    assert b.presence(v["agent"]) == (True, True)
    b.force_last_seen(v["agent"], v["stale_seconds"])
    assert b.presence(v["agent"]) == (False, True)


def run_d1(v: dict) -> None:
    b = InboxBinding()
    b.register(v["recipient"])
    commit = b.stage_delivery(v["recipient"], msg(v["message"]))
    assert b.poll(v["recipient"]) == []
    commit()
    got = b.poll(v["recipient"])
    assert len(got) == 1 and got[0].body == v["message"]["body"]


def run_d2(v: dict) -> None:
    b = InboxBinding()
    b.register(v["recipient"])

    def sender(i: int) -> None:
        from_id = f"{v['sender_prefix']}{i % v['sender_modulo']}"
        b.deliver(v["recipient"], Msg(from_id=from_id, body=f"{v['body_prefix']}{i}"))

    threads = [threading.Thread(target=sender, args=(i,)) for i in range(v["count"])]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert len(b.poll(v["recipient"])) == v["count"]


def run_o1(v: dict) -> None:
    b = InboxBinding()
    b.register(v["recipient"])
    for body in v["bodies"]:
        b.deliver(v["recipient"], Msg(from_id=v["sender"], body=body))
    assert [m.body for m in b.poll(v["recipient"])] == v["bodies"]


def run_c1(v: dict) -> None:
    b = InboxBinding()
    b.register(v["recipient"])
    b.deliver(v["recipient"], msg(v["message"]))
    acts: list[str] = []

    def act(m: Msg) -> None:
        acts.append(m.body)

    b.crash_after_act_once()
    try:
        b.consume(v["recipient"], act)
    except RuntimeError:
        pass
    assert acts == [v["message"]["body"]]
    assert b.consume(v["recipient"], act) == 1
    assert acts == [v["message"]["body"], v["message"]["body"]]
    deduper = Deduper()
    effects = 0

    def effect(_: Msg) -> None:
        nonlocal effects
        effects += 1

    for _ in acts:
        deduper.once(Msg(key=v["message"]["key"]), effect)
    assert effects == 1


def run_e1(v: dict) -> None:
    b = InboxBinding()
    b.register(v["recipient"])
    b.deliver(v["recipient"], msg(v["message"]))
    got = b.poll(v["recipient"])[0]
    assert got.from_id == v["message"]["from"] and got.body == v["message"]["body"]
    try:
        b.deliver(v["recipient"], msg(v["invalid_message"]))
    except ValueError:
        return
    raise AssertionError("message without from must be refused")


def run_b1(v: dict) -> None:
    b = InboxBinding()
    b.register(v["recipient"])
    b.deliver(v["recipient"], msg(v["message"]))
    assert b.private_bodies()
    assert b.peek_bodies(v["recipient"], v["reader"]) == []


RUNNERS = {
    "presence_freshness": run_r1,
    "atomic_visibility": run_d1,
    "no_overwrite": run_d2,
    "per_sender_fifo": run_o1,
    "consume_redelivery": run_c1,
    "envelope": run_e1,
    "body_privacy": run_b1,
}


def main() -> int:
    for vector in load_vectors().values():
        RUNNERS[vector["kind"]](vector)
        print(f"ok {vector['id']} {vector['kind']}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
