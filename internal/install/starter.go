package install

// StarterConfig is written to ~/.config/iris/config.yaml on first install.
const StarterConfig = `# Iris credential broker config.
# The sandbox cannot read this file. After editing, run: iris install   (regenerates Claude-side configs)
# Easiest way to add identities: iris provision kube|gcp|aws|github ...

approval:
  timeout: 60s               # unanswered prompt = deny
  options: [once, 15m, 1h, session]   # buttons, shortest first (first is the default)
  sessionMax: 8h             # cap for "until session end"
  touchID: false             # require Touch ID in Iris.app before granting

proxy:
  enabled: false             # header-injecting egress proxy for curl / HTTP APIs
  listen: 127.0.0.1:18899
  allowedHosts: []           # empty = allow all. Claude's own domain allowlist is bypassed when this proxy is on.

identities: []
# Examples (prefer narrow, Claude-only identities: see "iris provision"):
#
# - tool: kube
#   target: staging              # context name Claude sees
#   sourceContext: staging-admin # your context whose cluster/CA is copied (default: target)
#   scope: read
#   namespace: default
#   maxTTL: 1h
#   source:
#     command: [kubectl, --context, staging-admin, -n, iris, create, token, claude-read, --duration, 1h]
#
# - tool: aws
#   target: claude-dev           # profile name inside Claude
#   region: eu-west-1
#   scope: read
#   source:
#     command: [aws, sts, assume-role, --role-arn, "arn:aws:iam::123456789012:role/iris-claude-read",
#               --role-session-name, iris, --duration-seconds, "3600", --output, json]
#
# - tool: gcp
#   target: my-project
#   scope: viewer
#   source:
#     command: [gcloud, auth, print-access-token, --impersonate-service-account,
#               claude-viewer@my-project.iam.gserviceaccount.com]
#
# - tool: git
#   target: github.com
#   username: x-access-token
#   scope: repo:read
#   source:
#     keychain: {service: iris, account: github.com}
#
# - tool: docker
#   target: ghcr.io
#   source:
#     op: op://Private/ghcr-claude/credential
#
# - tool: http                    # needs proxy.enabled: true
#   target: api.github.com
#   header: Authorization
#   valueTemplate: "Bearer {{token}}"
#   source:
#     keychain: {service: iris, account: github.com}
`
