// Run with `bun test` from worker/. Stands in for the Workers runtime pieces
// the hub uses (WebSocketPair, the object's socket list and storage,
// timingSafeEqual, a Response that can carry a socket), then drives the
// worker through fetch and the object's socket handlers.

import { beforeEach, describe, expect, test } from "bun:test";
import { timingSafeEqual } from "node:crypto";
import worker, { Hub } from "../src/index.js";

crypto.subtle.timingSafeEqual ??= (a, b) => timingSafeEqual(a, b);

class FakeSocket {
  sent = [];
  closed = null;
  accept() {}
  send(text) {
    if (this.closed) throw new Error("closed");
    this.sent.push(JSON.parse(text));
  }
  close(code, reason) {
    this.closed = { code, reason };
  }
  last(t) {
    return this.sent.findLast((f) => f.t === t);
  }
}

// Every pair made, so a test can look at a socket the hub refused.
const pairs = [];
globalThis.WebSocketPair = class {
  constructor() {
    this[0] = new FakeSocket();
    this[1] = new FakeSocket();
    pairs.push(this);
  }
};
globalThis.WebSocketRequestResponsePair = class {};

// The runtime's Response takes status 101 and a socket. The one here doesn't.
const RealResponse = Response;
globalThis.Response = class extends RealResponse {
  constructor(body, init = {}) {
    if (init.status !== 101) return new RealResponse(body, init);
    super(null);
    this.upgrade = init.webSocket;
  }
};

function fakeContext() {
  const sockets = [];
  const store = new Map();
  return {
    sockets,
    store,
    setWebSocketAutoResponse() {},
    acceptWebSocket(ws, tags) {
      sockets.push({ ws, tags });
    },
    // Like the runtime, this keeps listing a socket after it closes.
    getWebSockets(tag) {
      return sockets.filter((s) => tag === undefined || s.tags.includes(tag)).map((s) => s.ws);
    },
    getTags(ws) {
      return sockets.find((s) => s.ws === ws)?.tags ?? [];
    },
    storage: {
      async get(key) {
        return store.get(key);
      },
      async put(key, value) {
        store.set(key, value);
      },
      async delete(key) {
        store.delete(key);
      },
      async list({ prefix }) {
        return new Map([...store].filter(([key]) => key.startsWith(prefix)));
      },
    },
  };
}

const TOKEN = "cth_test";
const ID_A = "aaaaaaaaaaaaaaaaaaaaaa";
const ID_B = "bbbbbbbbbbbbbbbbbbbbbb";

let ctx, hub, env;

beforeEach(() => {
  ctx = fakeContext();
  hub = new Hub(ctx);
  env = { HUB_TOKEN: TOKEN, HUB: { idFromName: (name) => name, get: () => hub } };
});

function request(query, headers = {}) {
  return new Request(`https://hub.example/connect?${query}`, {
    headers: { Authorization: `Bearer ${TOKEN}`, Upgrade: "websocket", ...headers },
  });
}

// Connects a device and returns the hub's end of its socket.
async function connect(name, id) {
  const before = ctx.sockets.length;
  const response = await worker.fetch(request(`v=1&name=${name}&id=${id}`), env);
  expect(response.upgrade).toBeDefined();
  return ctx.sockets.length > before ? ctx.sockets.at(-1).ws : null;
}

describe("the worker", () => {
  test("refuses everything until HUB_TOKEN is set", async () => {
    const response = await worker.fetch(request("v=1&name=a&id=" + ID_A), { ...env, HUB_TOKEN: "" });
    expect(response.status).toBe(503);
  });

  test("answers the same without the token, whatever is asked", async () => {
    for (const r of [
      request("v=1&name=a&id=" + ID_A, { Authorization: "Bearer wrong" }),
      new Request("https://hub.example/anything"),
    ]) {
      const response = await worker.fetch(r, env);
      expect(response.status).toBe(401);
    }
    expect(ctx.sockets).toHaveLength(0);
  });

  test("turns away what is not a connect", async () => {
    const cases = [
      [new Request("https://hub.example/", { headers: { Authorization: `Bearer ${TOKEN}` } }), 404],
      [request("v=1&name=a&id=" + ID_A, { Upgrade: "" }), 426],
      [request("v=2&name=a&id=" + ID_A), 400],
      [request("v=1&name=Not%20A%20Name&id=" + ID_A), 400],
      [request("v=1&name=a&id=short"), 400],
    ];
    for (const [r, status] of cases) expect((await worker.fetch(r, env)).status).toBe(status);
  });
});

describe("the hub", () => {
  test("tells every device who is connected", async () => {
    const a = await connect("lychee", ID_A);
    const b = await connect("macbook", ID_B);
    for (const ws of [a, b]) {
      expect(ws.last("devices").devices.map((d) => [d.name, d.online])).toEqual([
        ["lychee", true],
        ["macbook", true],
      ]);
    }
  });

  test("passes a box to its device, unread, with who sent it", async () => {
    const a = await connect("lychee", ID_A);
    const b = await connect("macbook", ID_B);
    await hub.webSocketMessage(a, JSON.stringify({ t: "msg", to: "macbook", ref: "r1", box: "sealed" }));
    expect(b.last("msg")).toEqual({ t: "msg", from: "lychee", box: "sealed" });
    expect(a.last("nack")).toBeUndefined();
  });

  test("answers a message for a device that is not connected", async () => {
    const a = await connect("lychee", ID_A);
    await hub.webSocketMessage(a, JSON.stringify({ t: "msg", to: "macbook", ref: "r1", box: "sealed" }));
    expect(a.last("nack")).toEqual({ t: "nack", ref: "r1", reason: "offline" });
  });

  test("refuses a frame over the limit and drops a malformed one", async () => {
    const a = await connect("lychee", ID_A);
    const b = await connect("macbook", ID_B);
    await hub.webSocketMessage(a, JSON.stringify({ t: "msg", to: "macbook", box: "x".repeat(600 * 1024) }));
    expect(a.last("nack")).toEqual({ t: "nack", reason: "too-large" });
    await hub.webSocketMessage(a, "not json");
    await hub.webSocketMessage(a, JSON.stringify({ t: "msg", to: "macbook" }));
    expect(b.last("msg")).toBeUndefined();
  });

  test("keeps a name for the device that first used it", async () => {
    await connect("lychee", ID_A);
    await worker.fetch(request(`v=1&name=lychee&id=${ID_B}`), env);
    expect(pairs.at(-1)[1].closed.code).toBe(4001);
    expect(ctx.sockets).toHaveLength(1);
  });

  test("lets a device reconnect, closing its older socket", async () => {
    const first = await connect("lychee", ID_A);
    const second = await connect("lychee", ID_A);
    expect(first.closed.code).toBe(4002);
    expect(second.last("devices").devices).toHaveLength(1);
  });

  test("shows a device as away once its connection ends", async () => {
    const a = await connect("lychee", ID_A);
    const b = await connect("macbook", ID_B);
    await hub.webSocketError(b);
    expect(a.last("devices").devices.map((d) => [d.name, d.online])).toEqual([
      ["lychee", true],
      ["macbook", false],
    ]);
  });

  test("removes a device from the list and closes its connection", async () => {
    const a = await connect("lychee", ID_A);
    const b = await connect("macbook", ID_B);
    await hub.webSocketMessage(a, JSON.stringify({ t: "remove", name: "macbook" }));
    expect(b.closed.code).toBe(4003);
    expect(a.last("devices").devices.map((d) => d.name)).toEqual(["lychee"]);
  });
});
