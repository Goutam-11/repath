# Cross-language HTTP fixture

A TS client and three servers (FastAPI, Flask, Express) with literal routes.
The tree-sitter graph has no cross-language answer on its own; the xlang pass
adds rule-tagged `REFERENCES` at 0.45.

| Client call | Server route | Expected |
| --- | --- | --- |
| `loadUsers` — `fetch("/api/users")` | FastAPI `GET /api/users` → `list_users` | **edge** |
| `createUser` — `fetch("/api/users", {method:"POST"})` | FastAPI `POST /api/users` → `create_user` | **edge** |
| `addItem` — `fetch("/api/items", {method:"POST"})` | Flask `POST /api/items` → `add_item` | **edge** |
| `loadProducts` — `fetch("/api/products")` | Express `GET /api/products` → `products` | none (same language) |
| `missing` — `fetch("/api/missing")` | — | none |
| `dynamic` — `` fetch(`/api/users/${id}`) `` | — | none (not a literal) |
| `wrongMethod` — `POST /api/health` | FastAPI `GET /api/health` | none (method mismatch) |
| `literal` — `const literal = "/api/users"` | — | none (not a call) |

The gate is `internal/index/xlang_test.go`: 11 anchors, exactly the 3 edges
above, no cross-language edge for the negatives, the default 0.5 floor hides
the links while 0.45 reveals them, and cold / incremental / second `--full`
xlang row sets are identical.

There is no producer and no server here: both languages are parsed by
tree-sitter, and the anchors are extracted from the source. Regenerate nothing;
edit the source only with the expectations above.
