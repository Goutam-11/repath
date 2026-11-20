# GraphML 1.0 schema set (vendored)

The official GraphML 1.0 schema is a **set of five files**: `graphml.xsd`
redefines `graphml-structure.xsd`, the attribute and parse-info schemas redefine
it too, and `graphml-structure.xsd` imports `xlink.xsd`. They are vendored here so
the opt-in export check can validate offline:

```sh
REPATH_XSD=1 go test ./internal/cli -run TestGraphMLMatchesXSDSchema -v
```

`make check` never runs it, and the test skips when `xmllint` is not installed.

## Provenance

The schema host (`graphml.graphdrawing.org`) answered every request with
"Out of service" on 2026-11-20 (the same outage #85 hit), so the files were
fetched from the Internet Archive snapshot of the same URLs:

| File | Archived source | SHA-256 (archived bytes) |
| --- | --- | --- |
| `graphml.xsd` | `https://web.archive.org/web/2023id_/http://graphml.graphdrawing.org/xmlns/1.0/graphml.xsd` | `c5d04fb93d579712aa375f3ace67fe9243d6aaa9b039576516722af2d9c5af52` |
| `graphml-structure.xsd` | `…/graphml-structure.xsd` | `7809123855d2ef634dea2b678055b6230607efb9addde55a06c4afd67075a04b` |
| `graphml-attributes.xsd` | `…/graphml-attributes.xsd` | `e13e2d5dc72c1d8eb758309f6b26570d7e060219277773892529ec97dddf36bb` |
| `graphml-parseinfo.xsd` | `…/graphml-parseinfo.xsd` | `6d025e0b809d95fbf296139fda8721591d483de8eee1b0174b4a1508aee792ca` |
| `xlink.xsd` | `…/xlink.xsd` | `4f76a2f3c026979b733f92d3f85c815723848b72731a72ef40a1709959b698ed` |

The **only** change from the archived bytes is the `schemaLocation` normalization
below, so the set compiles from this directory without network access. No
element, attribute, group, or type definition was edited.

```
graphml.xsd             schemaLocation="graphml-structure.xsd"   (was the absolute graphml.graphdrawing.org URL)
graphml-structure.xsd   schemaLocation="xlink.xsd"               (same)
graphml-attributes.xsd  schemaLocation="graphml-structure.xsd"   (same)
graphml-parseinfo.xsd   schemaLocation="graphml-structure.xsd"   (same)
```

Re-vendoring later (when the official host returns) only needs the five files
from `http://graphml.graphdrawing.org/xmlns/1.0/` and the same normalization;
compare the archived hashes above to confirm the snapshot is unchanged.
