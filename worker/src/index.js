// crosstalk hub: passes sealed messages between one person's machines.
//
// GET /connect?v=1&name=<device>&id=<device id>
//     (Authorization: Bearer <HUB_TOKEN>, Upgrade: websocket)
//
// Every machine's daemon holds one WebSocket open here. A frame is JSON text:
//
//   from a device   {"t":"msg","to":<device>,"ref":<any>,"box":<sealed>}
//                   {"t":"remove","name":<device>}
//   to a device     {"t":"msg","from":<device>,"box":<sealed>}
//                   {"t":"nack","ref":<same>,"reason":"offline"|"too-large"}
//                   {"t":"devices","devices":[{"name","online","lastSeen"}]}
//
// The hub never opens a box: it is encrypted between the devices with a key
// the hub does not have. HUB_TOKEN is derived from that key one way, so it
// lets a device in without letting the hub read anything.
//
// Nothing is queued. A message for a device that is not connected is
// answered with a nack and dropped.
//
// The worker checks the token before it looks at anything else, so without
// one every request gets the same answer.

const PROTOCOL = "1";
const NAME = /^[a-z0-9][a-z0-9-]{0,31}$/;
const ID = /^[A-Za-z0-9_-]{16,64}$/;
// A 256 KB body, sealed and base64'd, with room for the envelope.
const MAX_FRAME = 512 * 1024;
const DEVICE_PREFIX = "device:";
// One object holds every connection. Its name never changes.
const HUB_NAME = "hub";

// Another machine already holds this name.
const CLOSE_NAME_TAKEN = 4001;
// The same device connected again, so its older socket goes.
const CLOSE_SUPERSEDED = 4002;
const CLOSE_REMOVED = 4003;

const enc = new TextEncoder();

export default {
  async fetch(request, env) {
    // Fail closed when the worker is deployed without its secret. Without
    // this, an unset HUB_TOKEN makes the expected header the literal string
    // "Bearer undefined", which anyone can send.
    if (!isNonEmptyString(env.HUB_TOKEN))
      return new Response("hub not configured: set HUB_TOKEN\n", { status: 503 });
    if (!authorized(request, env)) return new Response("unauthorized\n", { status: 401 });

    const url = new URL(request.url);
    if (request.method !== "GET" || url.pathname !== "/connect")
      return new Response("not found\n", { status: 404 });
    if (request.headers.get("Upgrade")?.toLowerCase() !== "websocket")
      return new Response("expected a websocket\n", { status: 426 });
    if (url.searchParams.get("v") !== PROTOCOL)
      return new Response("unsupported protocol version, update crosstalk\n", { status: 400 });
    if (!NAME.test(url.searchParams.get("name") ?? "") || !ID.test(url.searchParams.get("id") ?? ""))
      return new Response("bad device name or id\n", { status: 400 });

    return env.HUB.get(env.HUB.idFromName(HUB_NAME)).fetch(request);
  },
};

export class Hub {
  constructor(ctx) {
    this.ctx = ctx;
    // Daemons ping every few seconds to notice a dead connection. The
    // runtime answers those itself, without waking a sleeping object.
    ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair("ping", "pong"));
  }

  // The worker has already checked the token and the shape of name and id.
  async fetch(request) {
    const url = new URL(request.url);
    const name = url.searchParams.get("name");
    const id = url.searchParams.get("id");

    // A name belongs to the first device id that connects with it, so two
    // machines that happen to share a name cannot take over each other's
    // messages.
    const key = DEVICE_PREFIX + name;
    const known = await this.ctx.storage.get(key);
    if (known && known.id !== id) return refuse(CLOSE_NAME_TAKEN, "name taken");

    // A reconnecting device never fights its own half-dead socket.
    const superseded = this.ctx.getWebSockets(name);
    const pair = new WebSocketPair();
    // The name rides as the socket's tag, so it survives the object sleeping.
    this.ctx.acceptWebSocket(pair[1], [name]);
    const now = Date.now();
    await this.ctx.storage.put(key, { id, firstSeen: known?.firstSeen ?? now, lastSeen: now });
    for (const ws of superseded) safeClose(ws, CLOSE_SUPERSEDED, "superseded by a newer connection");
    await this.announce(new Set(superseded));
    return new Response(null, { status: 101, webSocket: pair[0] });
  }

  async webSocketMessage(ws, message) {
    if (typeof message !== "string") return;
    const from = this.ctx.getTags(ws)[0];
    if (from === undefined) return;
    if (message.length > MAX_FRAME) {
      // Too large to be worth parsing for its ref, so the sender is told
      // without one and matches it to its own oversized send.
      safeSend(ws, { t: "nack", reason: "too-large" });
      return;
    }
    // A malformed frame is dropped, never fatal: one bad message must not
    // tear down a socket carrying live traffic.
    let frame;
    try {
      frame = JSON.parse(message);
    } catch {
      return;
    }
    if (frame === null || typeof frame !== "object") return;

    if (frame.t === "msg") {
      if (typeof frame.to !== "string" || typeof frame.box !== "string") return;
      const targets = this.ctx.getWebSockets(frame.to);
      if (targets.length === 0) {
        safeSend(ws, { t: "nack", ref: frame.ref, reason: "offline" });
        return;
      }
      for (const target of targets) safeSend(target, { t: "msg", from, box: frame.box });
    } else if (frame.t === "remove") {
      if (typeof frame.name !== "string") return;
      await this.ctx.storage.delete(DEVICE_PREFIX + frame.name);
      const closing = this.ctx.getWebSockets(frame.name);
      for (const target of closing) safeClose(target, CLOSE_REMOVED, "removed");
      await this.announce(new Set(closing));
    }
  }

  async webSocketClose(ws) {
    await this.departed(ws);
  }

  // The runtime calls this, not webSocketClose, when a connection dies
  // without a close frame (a dropped network, a machine going to sleep).
  async webSocketError(ws) {
    await this.departed(ws);
  }

  async departed(ws) {
    const name = this.ctx.getTags(ws)[0];
    if (name !== undefined) {
      const key = DEVICE_PREFIX + name;
      const known = await this.ctx.storage.get(key);
      if (known) await this.ctx.storage.put(key, { ...known, lastSeen: Date.now() });
    }
    await this.announce(new Set([ws]));
  }

  // Tells every connected device who is on the list and who is connected.
  // getWebSockets can still list sockets that just closed or are closing,
  // so callers name the ones that are going instead of trusting the listing.
  async announce(exclude) {
    const sockets = this.ctx.getWebSockets().filter((ws) => !exclude.has(ws));
    const online = new Set(sockets.map((ws) => this.ctx.getTags(ws)[0]));
    const stored = await this.ctx.storage.list({ prefix: DEVICE_PREFIX });
    const devices = [...stored]
      .map(([key, device]) => {
        const name = key.slice(DEVICE_PREFIX.length);
        return { name, online: online.has(name), lastSeen: device.lastSeen };
      })
      .sort((a, b) => (a.name < b.name ? -1 : 1));
    for (const ws of sockets) safeSend(ws, { t: "devices", devices });
  }
}

// Completes the upgrade, then closes with the code. A refusal before the
// upgrade would reach the daemon as a bare HTTP status with no reason.
function refuse(code, reason) {
  const pair = new WebSocketPair();
  pair[1].accept();
  pair[1].close(code, reason);
  return new Response(null, { status: 101, webSocket: pair[0] });
}

// A socket can die between being listed and being written to, and a dead
// one that throws must not take the sender's socket down with it.
function safeSend(ws, frame) {
  try {
    ws.send(JSON.stringify(frame));
  } catch {
    // Dropped on purpose, see above.
  }
}

function safeClose(ws, code, reason) {
  try {
    ws.close(code, reason);
  } catch {
    // Already gone.
  }
}

function authorized(request, env) {
  const auth = request.headers.get("Authorization") ?? "";
  return timingSafeEqual(auth, `Bearer ${env.HUB_TOKEN}`);
}

function timingSafeEqual(a, b) {
  const ab = enc.encode(a);
  const bb = enc.encode(b);
  if (ab.length !== bb.length) return false;
  return crypto.subtle.timingSafeEqual(ab, bb);
}

function isNonEmptyString(v) {
  return typeof v === "string" && v.length > 0;
}
