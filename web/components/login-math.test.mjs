import assert from "node:assert/strict";
import { describe, it } from "node:test";

import { shouldShowLogin, loginErrorText } from "./login-math.js";

describe("shouldShowLogin", () => {
  it("never blocks open mode", () => {
    assert.equal(shouldShowLogin({ authenticated: false, mode: "open" }), false);
  });
  it("never blocks an authenticated session", () => {
    assert.equal(shouldShowLogin({ authenticated: true, mode: "required" }), false);
  });
  it("blocks login-required modes without a session", () => {
    assert.equal(shouldShowLogin({ authenticated: false, mode: "local_trust" }), true);
    assert.equal(shouldShowLogin({ authenticated: false, mode: "required" }), true);
  });
  it("does not block on missing payloads", () => {
    assert.equal(shouldShowLogin(null), false);
    assert.equal(shouldShowLogin(undefined), false);
    assert.equal(shouldShowLogin({}), false);
  });
});

describe("loginErrorText", () => {
  it("keeps bad credentials uniform", () => {
    assert.equal(loginErrorText(401), "Wrong username or password.");
  });
  it("maps throttling and server errors", () => {
    assert.match(loginErrorText(429), /many attempts/);
    assert.match(loginErrorText(500), /Server error/);
    assert.equal(loginErrorText(400), "Login failed.");
  });
});
