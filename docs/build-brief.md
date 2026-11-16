# Credential broker for Claude Code — build brief

Nov 16, 2026 · @Joost

## Goal and threat model

Local Claude Code should only use my credentials (kubectl, gcloud, aws, git, curl to APIs) after I approve a popup that says what it wants, and that approval should expire after a set time.

Claude Code's built-in allow rules can't do this, because they match command strings, not identities. Claude can run `cat ~/.kube/config`, `python -c`, or `env` to read a credential and then use it some other way. A gate only works if the agent process cannot read the credential at all and has to ask a broker for it.

Threats in scope: a prompt injection or a confused agent using my credentials for an unintended action, or exfiltrating them. Out of scope: a compromised host, or me approving something malicious.

## Architecture

There are three layers. The sandbox hides the credentials, the broker owns them and asks me, and the CLIs reach the broker through their standard credential-helper hooks.

```
Claude Code (sandboxed bash)
  └─ kubectl / gcloud / aws / git / docker
       └─ credential helper (tiny CLI, runs in sandbox)
            └─ Unix socket ──► broker daemon (host, outside sandbox)
                                 ├─ checks grants file (TTL)
                                 ├─ no grant → native popup / Touch ID
                                 └─ returns short-lived token
curl / raw HTTP ──► sandbox egress proxy ──► injects Authorization header if granted
PreToolUse hook ──► writes pending command → broker shows it in the popup
```

1. **Sandbox (native, config only).** Turn on `sandbox.enabled` and list `~/.kube`, `~/.config/gcloud`, `~/.aws`, `~/.ssh`, `~/.docker/config.json`, and secret env vars in `sandbox.credentials` (Claude Code v2.1.187+). Disable the unsandboxed-retry fallback.
2. **Broker daemon (build).** It holds the real credentials or can mint them. It prompts, records grants with an expiry, and serves tokens over a Unix socket that the sandbox is allowed to reach.
3. **Helpers (build, thin).** One per tool, each plugged into the tool's existing auth hook (see Components).
4. **Egress proxy (build or adopt).** For curl and HTTP APIs, the proxy injects headers for approved hosts so the token never enters the sandbox. Prior art: openmoat, Foundry Sandbox.
5. **PreToolUse hook (optional).** Gives the popup context only ("`kubectl delete pod x`"). It is never the enforcement point.

## Components

Each tool already has a standard hook for an external credential helper, so nothing needs to intercept syscalls or shell commands.

| Tool | Hook | Helper returns | TTL handled by |
| --- | --- | --- | --- |
| kubectl | kubeconfig `users[].user.exec` (client.authentication.k8s.io/v1) | ExecCredential JSON with token + `expirationTimestamp` | kubectl caches until expiry |
| gcloud | `CLOUDSDK_AUTH_ACCESS_TOKEN_FILE` / `auth/access_token_file` | short-lived access token (SA impersonation) | broker rewrites or deletes the file |
| AWS CLI / SDKs | `credential_process` in `~/.aws/config` | JSON with STS creds + `Expiration` | SDK refreshes after expiry |
| git / gh | git credential helper | username + fine-grained token | broker |
| docker | `credsStore` / `credHelpers` | registry token | broker |
| curl / HTTP | sandbox proxy (custom proxy setting) | header injected per host | proxy grant table |

What to build:

- **broker daemon**: Go or Swift, launchd agent, Unix socket, macOS dialog with LocalAuthentication (Touch ID). Pulls secrets from 1Password (`op`) or the Keychain.
- **helper binary**: one binary with subcommands (`broker-helper kube <context>`, `gcp <project>`, `aws <profile>`, `git`). It only talks to the socket.
- **grants store**: in-memory plus an append-only audit log at `~/.local/state/broker/audit.jsonl`.
- **config**: maps each kube context, gcp project, or aws profile to a secret source and default TTL.
- **Claude-only identities** (non-code, big win): read-only k8s RBAC user, narrow GCP service account, fine-grained GitHub PAT. The broker hands out these, not my personal credentials.

## Grant model and UX

A grant is (tool, target, scope) plus an expiry, for example `kubectl · staging · read · 15 min`.

- Popup shows: tool, target (context, project, profile, host), scope, the pending command if the hook supplied one, and the requesting process (pid, cwd).
- Buttons: Deny · Once · 15 min · 1 h · until session end. The default is the shortest option.
- Scope comes from which identity is handed out (read-only vs admin SA), not from parsing the command.
- Menu bar or CLI shows active grants with revoke-all. Every issue and deny goes to the audit log.
- A timeout with no answer means deny.

Non-goals: parsing shell commands for intent, protecting against a compromised host, Windows.

Open questions:

- Can a sandboxed process reach a Unix socket outside the sandbox on macOS Seatbelt, and which sandbox setting allows it?
- Does the egress proxy need TLS termination (local CA in the sandbox) for header injection, or can tools use plain HTTP to the proxy?
- How should the broker tell a Claude-originated request apart from my own terminal? Options: separate kubeconfig or env only inside Claude sessions, or a parent-process check.

## Milestones

Build in four steps, kubectl first, because its exec plugin gives TTL caching for free.

1. **Lockdown.** Enable the sandbox and `sandbox.credentials`, then verify Claude can't read `~/.kube/config`, `~/.config/gcloud`, or `~/.aws`.
2. **Broker + kubectl.** Daemon, socket, popup, grants, and the kube exec helper on one staging context.
3. **gcloud, AWS, git.** Add helpers, and create Claude-only identities with narrow permissions.
4. **HTTP proxy and hook.** Header-injecting egress proxy for curl, plus the PreToolUse hook for popup context.

Acceptance tests (run from inside a Claude Code session):

- [ ] `cat ~/.kube/config` and `env | grep TOKEN` return nothing useful
- [ ] `kubectl get pods` shows a popup; Deny makes it fail; 15 min makes repeat calls silent until expiry
- [ ] after the TTL, the next call prompts again
- [ ] revoke-all makes the next call prompt immediately
- [ ] `curl` to an approved API works without the token appearing in the sandbox
- [ ] the audit log has one line per issue or deny
- [ ] my own terminal outside Claude is unaffected
