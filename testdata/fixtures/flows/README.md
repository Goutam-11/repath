# Entrypoint/flow fixtures

Small per-language trees that exercise the documented entrypoint heuristics:

- TypeScript: an Express `app.get("/start", start)` registration (the handler
  calls `step()` then `finish()`) and a Next.js `app/users/route.ts` `GET`.
- Python: FastAPI `@router.get`, Flask `@app.route`, a pytest `test_` function,
  and the `if __name__ == "__main__"` guard around `main()`.
- Rust: `fn main` and a `#[test]` function.

They are deliberately tiny and only test detection/flow traversal, not
framework semantics.
