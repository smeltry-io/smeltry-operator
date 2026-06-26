# ADR-002 — OIDC Authentication via Authentik

**Status**: Accepted  
**Date**: 2026-06-26  
**Deciders**: @mgrzybek

---

## Context

Smeltry exposes Kubernetes resources (`ClusterClaim`, `ServerClaim`, etc.) directly
via the kube-apiserver. Every client — the Headlamp web UI, the `smeltry` CLI, and
any automation — authenticates using OIDC tokens issued by Authentik.

No custom backend API sits between the clients and the kube-apiserver. The
kube-apiserver validates tokens natively using its built-in OIDC middleware (see
CLAUDE.md §3.1). A single OIDC issuer (Authentik) serves all clients.

---

## Decision

### Two Authentik OIDC providers, one kube-apiserver client-id

| Provider | Type | Grant types | Purpose |
|----------|------|-------------|---------|
| `smeltry-cli` | Public | `device_code`, `authorization_code`, `refresh_token` | CLI (`smeltry`) and kube-apiserver OIDC flag |
| `smeltry-headlamp` | Confidential | `authorization_code`, `refresh_token` | Headlamp web plugin |

Both providers share the same `client_id` value (`smeltry-cli`) as seen by the
kube-apiserver `--oidc-client-id` flag, or they are configured so the JWT `aud`
claim matches `smeltry-cli`. Only one `--oidc-client-id` can be set on the
kube-apiserver.

### kube-apiserver OIDC flags (management cluster and each TenantControlPlane)

```
--oidc-issuer-url=https://auth.<domain>/application/o/smeltry/
--oidc-client-id=smeltry-cli
--oidc-username-claim=email
--oidc-username-prefix=oidc:
--oidc-groups-claim=groups
--oidc-groups-prefix=
```

These values are stored in `SiteConfig.spec.oidc` and injected by
`smeltry-operator` into each `TenantControlPlane` (Kamaji) at creation time.
The management cluster's kube-apiserver must be configured manually or via the
cluster bootstrap tooling (out of scope for the operator).

### Authentik groups scope — required Property Mapping

Authentik does not expose a `groups` claim by default. A custom Scope Mapping
must be created:

```
Customization → Property Mappings → Scope Mapping
  Scope name : groups
  Expression :
    return [group.name for group in request.user.ak_groups.all()]
```

Both providers must reference this Scope Mapping so that every issued JWT
contains `"groups": ["<slug>", ...]`.

### RBAC convention

The `groups` claim value is the Authentik group name, which equals the Netbox
tenant slug, which equals the Kubernetes namespace suffix:

```
Authentik group "acme"  →  JWT groups: ["acme"]  →  namespace "tenant-acme"
```

`NetboxTenantReconciler` creates one `RoleBinding` per namespace that binds the
group slug to the `cluster-user` Role. The kube-apiserver applies this binding
because `--oidc-groups-prefix=""` means group names in the JWT match the
Kubernetes group names in `RoleBinding.subjects` directly.

### Cluster-scoped ClusterRoles (managed by smeltry-operator at startup)

| ClusterRole | Bound to | Rights |
|-------------|----------|--------|
| `smeltry-admin` | Group `smeltry-admins` (Authentik) | Full access to `portal.smeltry.io/*` |
| `smeltry-catalog-reader` | `system:authenticated` | Read `addonprofiles`, `siteconfigs` in `portal-system` |

These ClusterRoles and their ClusterRoleBindings are idempotently created by
`smeltry-operator` at startup via `internal/rbac.EnsureClusterRBAC()`.

---

## Consequences

### Positive
- No custom auth middleware to maintain: the kube-apiserver validates JWTs natively.
- Tenant isolation is enforced by Kubernetes RBAC, not by application code.
- The CLI uses the standard OAuth2 device flow — no browser required for headless use.
- Group membership changes in Authentik are reflected immediately (no caching layer).

### Negative / risks
- A single `--oidc-issuer-url` per kube-apiserver: rotating the Authentik domain
  requires a rolling restart of all control planes.
- The `groups` claim is unbounded: a user in many Authentik groups will have a
  large JWT. Mitigate by keeping Authentik groups scoped to tenant slugs only.
- The management cluster's kube-apiserver OIDC flags must be set at bootstrap
  time; changing them requires a control-plane restart.

---

## Rejected alternatives

### Multiple OIDC issuers
Kubernetes supports only one `--oidc-issuer-url`. Supporting multiple IDPs would
require an OIDC proxy (e.g., Dex) in front of Authentik — added complexity with
no benefit for a single-IDP deployment.

### Custom auth webhook
A `--authentication-token-webhook-config-file` would allow arbitrary token
validation, but adds a latency-sensitive dependency on a custom service that must
be highly available. Not justified when Authentik is already the authoritative IDP.
