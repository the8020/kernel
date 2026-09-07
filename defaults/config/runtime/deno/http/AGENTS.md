Parent DOX: [kernel/defaults/config/runtime/deno DOX](../AGENTS.md).

# Purpose

- Provide the Phase 1C TypeScript HTTP service framework exposed as
  `@the8020/http`.

# Ownership

- Own Hono-backed relative HTTP and WebSocket route registration, Zod request
  validation and inference, service-local middleware, structured HTTP errors,
  standard `Response` dispatch, and deterministic OpenAPI generation.
- Export `defineService`, `z`, public HTTP/WebSocket handler, middleware, and
  schema types, `HTTPError`, and the narrow runtime service contract consumed by
  the Worker bootstrap.
- Do not own canonical public routing, service discovery, shared desired state,
  Worker pools, sandbox lifecycle, or supervisor authentication.

# Local Contracts

- Entrypoints default-export the object returned by `defineService()`; routes
  never include the filesystem-derived canonical prefix.
- Declared params, query, headers, and JSON bodies are validated before
  handlers; undeclared bodies remain streaming and unbuffered.
- Handlers return standard web `Response` objects. Validation failures are
  stable `400` JSON responses and uncaught failures are generic `500` responses
  associated with request identity.
- Response compression is negotiated by the supervisor's native Deno HTTP
  server. Handlers return ordinary bodies; set `Content-Encoding` only when the
  body is already encoded. A handler or existing service middleware can append
  `no-transform` to `Cache-Control` to exclude its responses from compression.
  Compression policy stays with response semantics, without a service-manifest
  option or a custom response wrapper.
- `service.websocket()` uses the same relative routes, middleware, parameters,
  query, and trusted request metadata as HTTP. It receives an abstract
  text/binary connection while the supervisor retains the physical socket. The
  private acceptance marker is `the8020-internal-websocket-accepted`; it never
  reaches public responses. Existing OpenAPI extension fields are not HTTP
  headers and retain their own names.
- Trusted metadata exposes authentication and generic current execution
  identity, including optional persistent execution identity, plus the
  kernel-observed client IP address and network scope; it carries no application
  configuration.
- OpenAPI paths are relative, servers contain the canonical base path, and
  output order follows deterministic registration order.
- The portable bundled module's self-types import and re-export the actual
  pinned Zod namespace. Preserve its complete API, schema identity, and
  inference; do not maintain a partial copy of dependency types. In-sandbox
  service validation must match source-tree type checking.
- `bundle-runtime.sh` publishes exactly `the8020_http.js` and
  `the8020_http.d.ts` in the generated HTTP output root and removes obsolete
  sibling build outputs before that root is staged into runtime images.

# Work Guidance

- Use the pinned `hono` and `zod` import aliases from the single runtime import
  map; do not add decorators, file discovery, or a proprietary response
  abstraction.

# Verification

- `http_test.ts` covers source/portable self-type metadata and full Zod API
  parity, shared schema composition and inference, methods, route
  order/patterns, parameters, query/header/body validation, middleware, standard
  and streaming responses, cancellation, structured errors, WebSocket
  text/binary handling, and deterministic relative HTTP/WebSocket OpenAPI
  output.

# Child DOX Index

No child DOX documents. This document owns the entire local scope.
