// Pure decision logic for <ftw-login-gate>, separated for node --test.

// shouldShowLogin decides whether the login overlay must block the app,
// from the /api/auth/session payload (or a fetch failure).
//
//   - open mode: never (session endpoint says mode "open").
//   - authenticated: never.
//   - 401 from any API implies a login-required mode: show.
//   - fetch failure (server restarting): don't block — the app's own
//     error states handle unreachable backends.
export function shouldShowLogin(session) {
  if (!session) return false;
  if (session.authenticated === true) return false;
  return session.mode === "local_trust" || session.mode === "required";
}

// loginErrorText maps a login response to the message shown under the
// form. Uniform for bad credentials (mirrors the API's no-oracle rule).
export function loginErrorText(status) {
  if (status === 401) return "Wrong username or password.";
  if (status === 429) return "Too many attempts — wait a moment.";
  if (status >= 500) return "Server error — try again.";
  return "Login failed.";
}
