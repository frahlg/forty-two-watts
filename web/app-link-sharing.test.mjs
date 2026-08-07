import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { describe, it } from "node:test";
import vm from "node:vm";

// Sharing, on the box's own settings page.
//
// The device list is where a household decides who can reach their home, so
// these drive the real refreshDevices() and the real buttons against a small
// DOM and read what they built. Asserting on the markup string alone would
// not catch the row that never gets a Remove button.

const source = readFileSync(new URL("./settings/tabs/app.js", import.meta.url), "utf8");

// A DOM small enough to read and real enough to answer the two questions
// here: what did the page build, and what does it say.
function element(tag) {
  const el = {
    tagName: tag,
    className: "",
    type: "",
    hidden: false,
    disabled: false,
    children: [],
    listeners: {},
    appendChild(child) {
      this.children.push(child);
      return child;
    },
    addEventListener(name, fn) {
      this.listeners[name] = fn;
    },
    click() {
      if (this.listeners.click) this.listeners.click();
    },
    // Everything this element and its descendants say, so a test can assert
    // on wording without walking the tree by hand.
    words() {
      return [this.textContent, ...this.children.map((c) => c.words())].join(" ");
    },
    buttons() {
      const mine = this.tagName === "button" ? [this] : [];
      return mine.concat(...this.children.map((c) => c.buttons()));
    },
  };

  // Setting textContent replaces everything inside, children included. That
  // is how the real DOM clears a slot, and it is what keeps the assertions
  // below about what is on screen now rather than about everything that has
  // ever been on it — a stale sentence left in the tree would let a test pass
  // on the strength of a code that has already been replaced.
  let text = "";
  Object.defineProperty(el, "textContent", {
    get() {
      return text;
    },
    set(value) {
      text = String(value);
      el.children = [];
    },
  });
  return el;
}

// The ids render() reaches for. Registered up front, because the tab wires
// itself to them the moment it is rendered.
const IDS = [
  "app-link-devices",
  "app-link-slot",
  "app-link-status",
  "app-link-pair",
  "app-link-share",
  "app-link-enabled",
];

// load runs app.js, renders the tab, and hands back the DOM it wired itself
// into along with every request it made.
function load({ devices = [], pairing = null, spoken = null, refuse = null } = {}) {
  const byId = new Map();
  for (const id of IDS) {
    byId.set(id, element(id.startsWith("app-link-devices") || id.endsWith("slot") ? "div" : "button"));
  }
  byId.get("app-link-share").hidden = true;

  const document = {
    getElementById: (id) => byId.get(id) ?? null,
    createElement: element,
  };

  const calls = [];
  const answer = (body, ok = true) => Promise.resolve({ ok, json: () => Promise.resolve(body) });

  const sandbox = {
    window: { FTWSettings: { tabs: {} } },
    setTimeout: (fn) => {
      fn();
      return 0;
    },
    document,
    confirm: () => true,
    fetch: (path, opts) => {
      calls.push({ path, opts });
      if (refuse && opts && opts.method === refuse.method) {
        return answer({ error: refuse.error }, false);
      }
      if (path === "/api/app-link/devices") return answer({ devices });
      if (path === "/api/app-link/pairing") {
        // The box echoes back the role it minted for, so the stub does too.
        // One that answered a fixed role whatever it was asked would hide the
        // bug this file exists to catch: a code whose screen names a power it
        // does not carry.
        const asked = JSON.parse((opts && opts.body) || "{}");
        const shape = asked.kind === "spoken" ? spoken : pairing;
        return answer(shape && Object.assign({}, shape, { role: asked.role }));
      }
      if (path === "/api/app-link/status") {
        return answer({ enabled: true, paired_devices: devices.length });
      }
      return answer({});
    },
    JSON,
    Promise,
    Date,
    Math,
    Array,
    Object,
    String,
    console,
  };
  sandbox.globalThis = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);

  const tab = sandbox.window.FTWSettings.tabs.app;
  const html = tab.render({ config: { app_link: { enabled: true } } });
  return { html, calls, el: (id) => byId.get(id) };
}

// Lets the promise chains inside the tab settle. Two turns, because a button
// press fetches and then repaints from a second fetch.
const settle = async () => {
  for (let i = 0; i < 4; i++) await new Promise((r) => setTimeout(r, 0));
};

describe("the device list", () => {
  it("says what each phone may do", async () => {
    const { el } = load({
      devices: [
        { id: "aaaa1111", role: "owner", last_seen_ms: Date.now() },
        { id: "bbbb2222", role: "viewer", last_seen_ms: Date.now() },
      ],
    });
    await settle();

    const list = el("app-link-devices");
    assert.equal(list.children.length, 2, "both phones should be listed");
    assert.match(list.children[0].words(), /Can change things/);
    assert.match(list.children[1].words(), /Can look/);
  });

  it("removes a guest with the same button that locks a phone out", async () => {
    const { el, calls } = load({
      devices: [
        { id: "aaaa1111", role: "owner" },
        { id: "bbbb2222", role: "viewer" },
      ],
    });
    await settle();

    const list = el("app-link-devices");
    for (const row of list.children) {
      const labels = row.buttons().map((b) => b.textContent);
      assert.ok(
        labels.includes("Remove"),
        `no Remove on "${row.words()}" — a guest must be removed where a phone is`,
      );
    }

    // And it is the same request, not a sharing-specific one.
    list.children[1].buttons().find((b) => b.textContent === "Remove").click();
    await settle();
    const removal = calls.find((c) => c.opts && c.opts.method === "DELETE");
    assert.ok(removal, "removing a guest sent nothing");
    assert.equal(removal.path, "/api/app-link/devices/bbbb2222");
  });

  it("explains the last owner instead of offering a button that fails", async () => {
    const { el } = load({
      devices: [{ id: "aaaa1111", role: "owner", last_owner: true }],
    });
    await settle();

    const row = el("app-link-devices").children[0];
    assert.equal(row.buttons().length, 0, "the last owner was offered a button the box refuses");
    assert.match(row.words(), /only phone that can change things/);
    assert.match(row.words(), /add another/i);
  });

  it("hides sharing until there is somebody to share from", async () => {
    // The first phone on a box is its owner whatever code it used, so an
    // invite offered before then would silently make an owner.
    const empty = load({ devices: [] });
    await settle();
    assert.equal(empty.el("app-link-share").hidden, true);

    const paired = load({ devices: [{ id: "aaaa1111", role: "owner" }] });
    await settle();
    assert.equal(paired.el("app-link-share").hidden, false);
  });

  it("changes a role from the list rather than a screen of its own", async () => {
    const { el, calls } = load({
      devices: [
        { id: "aaaa1111", role: "owner" },
        { id: "bbbb2222", role: "viewer" },
      ],
    });
    await settle();

    const row = el("app-link-devices").children[1];
    const promote = row.buttons().find((b) => /change things/.test(b.textContent));
    assert.ok(promote, "a guest cannot be promoted from the list");
    promote.click();
    await settle();

    const patch = calls.find((c) => c.opts && c.opts.method === "PATCH");
    assert.ok(patch, "no role change was sent");
    assert.equal(patch.path, "/api/app-link/devices/bbbb2222");
    assert.equal(JSON.parse(patch.opts.body).role, "owner");
  });

  it("says why when the box refuses", async () => {
    // Without this the button simply does nothing and the household has no
    // idea the box protected anything.
    const { el } = load({
      devices: [
        { id: "aaaa1111", role: "owner" },
        { id: "bbbb2222", role: "owner" },
      ],
      refuse: { method: "DELETE", error: "that is the only phone that can change anything here." },
    });
    await settle();

    const row = el("app-link-devices").children[0];
    row.buttons().find((b) => b.textContent === "Remove").click();
    await settle();

    assert.match(row.words(), /only phone that can change anything/);
  });
});

describe("the code on screen", () => {
  it("names what an invite lets in, above the code", async () => {
    const { el } = load({
      devices: [{ id: "aaaa1111", role: "owner" }],
      pairing: {
        url: "https://app.ftw.energy/p#v2.a.b.c.d",
        role: "viewer",
        expires_at_ms: Date.now() + 600000,
      },
    });
    await settle();

    el("app-link-share").click();
    await settle();

    const said = el("app-link-slot").words();
    assert.match(said, /lets someone see this home/i);
    assert.match(said, /cannot change anything/i);
  });

  it("asks for the role the button promises", async () => {
    const { el, calls } = load({
      devices: [{ id: "aaaa1111", role: "owner" }],
      pairing: { url: "https://app.ftw.energy/p#x", role: "viewer", expires_at_ms: Date.now() + 600000 },
    });
    await settle();

    el("app-link-share").click();
    await settle();
    const invite = calls.filter((c) => c.path === "/api/app-link/pairing").pop();
    assert.equal(JSON.parse(invite.opts.body).role, "viewer");

    el("app-link-pair").click();
    await settle();
    const owner = calls.filter((c) => c.path === "/api/app-link/pairing").pop();
    assert.equal(JSON.parse(owner.opts.body).role, "owner");
  });

  it("shows a box code as readable text", async () => {
    const { el, calls } = load({
      devices: [{ id: "aaaa1111", role: "owner" }],
      pairing: { url: "https://app.ftw.energy/p#x", expires_at_ms: Date.now() + 600000 },
      spoken: { code: "ABCD-EFGH", expires_at_ms: Date.now() + 300000 },
    });
    await settle();

    el("app-link-pair").click();
    await settle();
    el("app-link-slot").buttons()[0].click();
    await settle();

    const asked = calls.filter((c) => c.path === "/api/app-link/pairing").pop();
    assert.equal(JSON.parse(asked.opts.body).kind, "spoken");

    const said = el("app-link-slot").words();
    assert.match(said, /ABCD-EFGH/, "the code was never shown");
    assert.match(said, /Five wrong tries/, "nothing warns that guessing burns the code");
  });

  // The box code is the floor that always works, so it has to reach a guest
  // too — a phone with no camera cannot be shared a home by scanning. It
  // carries the role of the code already on screen, so the sentence the
  // household just read is the one the code obeys.
  it("reads out a code for whoever the code on screen was for", async () => {
    const { el, calls } = load({
      devices: [{ id: "aaaa1111", role: "owner" }],
      pairing: { url: "https://app.ftw.energy/p#x", expires_at_ms: Date.now() + 600000 },
      spoken: { code: "K7M2-9QRT", expires_at_ms: Date.now() + 300000 },
    });
    await settle();

    // The guest's code first, then read it out rather than scan it.
    el("app-link-share").click();
    await settle();
    const fallback = el("app-link-slot").buttons()[0];
    assert.ok(fallback, "a code on screen offers no way to read it out");
    assert.match(fallback.textContent, /read a code out/i);

    fallback.click();
    await settle();

    const asked = JSON.parse(calls.filter((c) => c.path === "/api/app-link/pairing").pop().opts.body);
    assert.deepEqual(
      asked,
      { role: "viewer", kind: "spoken" },
      "the spoken code was minted for a different role than the screen promised",
    );
    assert.match(el("app-link-slot").words(), /K7M2-9QRT/);
    assert.match(
      el("app-link-slot").words(),
      /cannot change anything/i,
      "the spoken code never says it is view only",
    );
  });

  // A typed code carries the code and nothing else. The box's own key and its
  // rendezvous secret travel only in the square, so a phone that has never
  // seen this box cannot be read its way in — and the page has to say so
  // rather than offer a path that cannot work.
  it("says a code read out will not do for a phone that has never been here", () => {
    const { html } = load({ devices: [{ id: "aaaa1111", role: "owner" }] });
    assert.match(html, /read out instead of scanned/i);
    assert.match(html, /never seen this box has to scan/i);
  });

  it("never prints a scannable payload as text", async () => {
    // The QR payload is a credential. The only path it should take is a
    // camera, so it must not appear anywhere a person could copy it.
    const payload = "https://app.ftw.energy/p#v2.secret.key.material.here";
    const { el } = load({
      devices: [{ id: "aaaa1111", role: "owner" }],
      pairing: { url: payload, role: "owner", expires_at_ms: Date.now() + 600000 },
    });
    await settle();

    el("app-link-pair").click();
    await settle();

    assert.doesNotMatch(el("app-link-slot").words(), /secret\.key\.material/);
  });
});
