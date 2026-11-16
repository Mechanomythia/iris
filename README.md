# Iris: a credential broker for Claude Code

Local Claude Code gets your credentials (kubectl, gcloud, aws, git, docker, curl to APIs) only after you approve a popup that says what it wants, and the approval expires.

```
Claude Code (sandboxed bash)                          your Mac, outside the sandbox
  kubectl / aws / gcloud / git / docker
    └─ iris-helper  ── broker.sock ──────────────►  irisd (launchd agent)
                                                     ├─ grant table (TTL, in memory)
                                                     ├─ no grant → Iris.app popup / Touch ID
  curl https://api…  ── httpProxyPort ───────────►  ├─ egress proxy: injects auth header
                                                     ├─ mints from 1Password / Keychain / command
  PreToolUse hook ── "kubectl delete pod x" ──────►  └─ audit.jsonl
                                                  ◄── control.sock ── Iris.app menu bar, `iris` CLI
```

The sandbox can't read `~/.kube`, `~/.aws`, `~/.config/gcloud`, `~/.ssh`, and so on, so the only way to use a credential is to ask the broker. The broker can't be approved from inside the sandbox: `control.sock` isn't in the sandbox socket allowlist, and it also refuses any process descended from Claude Code.

## Pieces

| | What | Runs |
|---|---|---|
| `iris` | CLI: `install` configures Claude Code, `provision` creates Claude-only identities, `daemon` is the broker, plus grants/revoke/audit/doctor | your terminal, launchd |
| `iris-helper` | tiny credential-helper binary, holds no secrets | inside the sandbox |
| `Iris.app` | SwiftUI menu bar app: approval popups, Touch ID, active grants, revoke-all, recent activity, one-click setup | your Mac |

## Install

From a release (built by CI): unzip `Iris-x.y.z.zip`, move `Iris.app` to `/Applications`, open it, then click **Set up Claude Code** in the menu. Or:

```bash
make install          # CLI only → ~/.local/bin/{iris,iris-helper}
```

```bash
iris install          # merges ~/.claude/settings.json, generates Claude-only configs, starts launchd agent
```

```bash
make app              # macOS: dist/Iris.app (needs Xcode CLT + Go)
```

`iris install` is idempotent and reversible: it writes a backup, records every entry it adds in `~/.config/iris/installed.json`, and `iris uninstall` removes exactly those entries. Re-run it after every config change; `iris provision` does that for you.

### What `iris install` changes in Claude Code

- `sandbox.enabled`, `failIfUnavailable: true`, `allowUnsandboxedCommands: false` (no escape hatch)
- `sandbox.credentials`: denies the credential files listed above, plus about 30 secret env vars (`AWS_*`, `GITHUB_TOKEN`, …)
- `permissions.deny`: `Read(~/.kube/**)` and friends, because Claude's Read tool is **not** sandboxed. Also `Edit(~/.config/iris/**)` and `Edit(~/.claude/settings.json)`, so Claude can't rewrite its own cage.
- `sandbox.network.allowUnixSockets: [~/.local/state/iris/broker.sock]`. This is the answer to the brief's open question 1.
- `env`: points `KUBECONFIG`, `AWS_CONFIG_FILE`, `CLOUDSDK_CONFIG`, `GIT_CONFIG_GLOBAL`, and `DOCKER_CONFIG` at generated Claude-only files in `~/.local/share/iris/claude/`. Your own terminal never sees these, so it's unaffected (open question 3).
- hooks: `PreToolUse` sends the pending command to the popup, `SessionStart` puts the shim dir on `PATH`, `SessionEnd` revokes "until session end" grants
- with the proxy enabled: `httpProxyPort` and a CA bundle (`SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, …)

## Claude-only identities: `iris provision`

Run these from **your** terminal. Each one creates a narrow identity with your credentials and adds it to the config. The broker then mints short-lived credentials for that identity, never for your personal one.

```bash
iris provision kube --context staging-admin --role view            # SA iris/claude-view + ClusterRoleBinding; tokens via `kubectl create token`
```

```bash
iris provision kube --context prod-admin --name prod --role edit --only-namespace app --ttl 15m
```

```bash
iris provision gcp --project my-proj --roles roles/viewer,roles/logging.viewer   # SA + roles + TokenCreator for you; tokens by impersonation
```

```bash
iris provision aws --profile admin --name claude-read --region eu-west-1         # role iris-claude-read (ReadOnlyAccess) trusted only by you; sts assume-role
```

```bash
iris provision github --owner acme --permissions contents:write,pull_requests:write --http   # opens a pre-filled fine-grained PAT page, stores token in Keychain
```

```bash
iris provision secret --tool http --target api.openai.com            # any API key → Keychain, injected by the proxy
```

Every provisioner supports `--dry-run`, which prints the exact `kubectl`/`gcloud`/`aws` commands and changes nothing.

## Using it

Inside Claude: `kubectl get pods` opens a popup with **Deny · Once · 15 min · 1 h · Until session end**. Return picks the shortest option, Escape denies, and no answer within 60 s denies.

```bash
iris status           # daemon, UI, identities, grants
```

```bash
iris revoke --all
```

```bash
iris audit -n 20
```

```bash
iris doctor           # host-side checks
```

Ask Claude to run `iris selftest`. It runs the acceptance checks from inside the sandbox: credential files unreadable, no secret env vars, broker reachable, control socket unreachable, Keychain not visible.

## Config (`~/.config/iris/config.yaml`)

```yaml
approval: {timeout: 60s, options: [once, 15m, 1h, session], sessionMax: 8h, touchID: true}
proxy: {enabled: true, listen: 127.0.0.1:18899, allowedHosts: ["*.github.com", "registry.npmjs.org"]}
identities:
  - tool: kube            # kube | aws | gcp | git | docker | http
    target: staging       # context | profile | project | git host | registry | API host
    scope: read           # label in the popup; real scope = the identity's permissions
    maxTTL: 1h            # caps both the credential and the longest button offered
    source:               # exactly one of:
      command: [kubectl, --context, staging-admin, -n, iris, create, token, claude-view, --duration, 1h]
      # op: op://Private/item/field
      # keychain: {service: iris, account: github.com}
      # env: NAME   /   file: ~/path
```

Source output is auto-detected: a raw token, a kube `ExecCredential`, AWS `credential_process` JSON, `sts assume-role` JSON, or `{"token": …, "expires_at": …}`. The credential handed out expires at whichever comes first, the source expiry or the grant expiry.

| Tool | Hook used | Notes |
|---|---|---|
| kubectl | kubeconfig `users[].user.exec` | kubectl caches until `expirationTimestamp` (= grant expiry) |
| aws | `credential_process` | SDKs refresh after `Expiration` |
| gcloud / bq / gsutil | PATH shim → `CLOUDSDK_AUTH_ACCESS_TOKEN_FILE` (temp file, deleted on exit) | gcloud has no exec hook. Google client libraries using ADC are **not** covered |
| git | credential helper (`helper =` reset + iris), SSH remotes rewritten to HTTPS | `password_expiry_utc` stops git caching past expiry |
| docker | `credHelpers` → `docker-credential-iris` | |
| curl / HTTP | proxy: TLS-terminated with the Iris CA, header replaced | `http://host` is upgraded to HTTPS upstream, so tools that can't trust the CA still work (open question 2) |

## Security model and known gaps

Same threat model as the brief: a prompt injection or a confused agent. A compromised host is out of scope, and so is you approving something bad. Honest list of gaps:

1. **Keychain visibility.** Items written by `security add-generic-password` can be read by `/usr/bin/security`. If Claude's Seatbelt profile allows the `securityd` mach lookup, sandboxed Bash could read them. `iris selftest` checks for this. If it fails, use `op://` sources: the 1Password CLI needs the app's socket, which the sandbox can't reach. A `Bash(security find-generic-password:*)` deny rule is installed, but it's a speed bump, not a wall.
2. **Approved credentials do enter the sandbox** for kube, aws, gcp, git, and docker. That's inherent to helper protocols. Only the proxy path keeps tokens out. Mitigations: short TTLs and narrow identities, which is why `provision` exists.
3. **A static secret outlives its grant.** A GitHub PAT handed out under a 15-minute grant is still valid at GitHub afterwards. Revoking stops Iris from handing it out again, not Claude from reusing a copy. Prefer minted sources (`kubectl create token`, impersonation, `assume-role`).
4. **The popup context is advisory.** Anything in the sandbox can send a fake "pending command". The panel says so, and the scope comes from the identity, never from parsing the command.
5. **The proxy bypasses Claude's domain allowlist** (setting `httpProxyPort` does that). Fill `proxy.allowedHosts`.
6. Grants live in memory: restarting the daemon revokes everything, by design.

## Development

```bash
make test
```

```bash
IRIS_HOME=/tmp/ih iris daemon     # isolated home for experiments
```

CI (`.github/workflows/build.yml`): Go vet and race tests on Linux, a darwin cross-compile, then on macOS darwin tests, a universal `Iris.app` (Swift + lipo'd Go), an ad-hoc signature, a CLI smoke test, and the zip uploaded as an artifact. Tags `v*` publish a GitHub release. Set `CODESIGN_IDENTITY` for Developer ID signing; notarization isn't wired up yet.
