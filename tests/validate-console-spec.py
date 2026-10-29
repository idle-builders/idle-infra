#!/usr/bin/env python3
"""Validate docs/api/console-v1.yaml, the infra-console contract.

That document is the single source for two generated artifacts (Go server stubs
and MCP tool definitions), so a malformed or under-specified contract breaks
both silently. The SPA is hand-written and can drift; only the generated pair
cannot. These checks run in
tests/validate.sh — see docs/plans/infra-console.md (AC1).

Emits one "ok<TAB>message" or "no<TAB>message" line per assertion; the caller
turns those into pass/fail counts. Exits non-zero if any assertion failed, so
it is also usable standalone.

    python3 tests/validate-console-spec.py docs/api/console-v1.yaml
"""

import sys

try:
    import yaml
except ImportError:  # pragma: no cover - environment problem, not a spec problem
    print("no\tPyYAML is not installed")
    sys.exit(1)

HTTP_METHODS = ("get", "post", "put", "patch", "delete", "head", "options", "trace")
KNOWN_VARIANTS = {"console", "agent"}

results = []


def check(condition, message):
    results.append((bool(condition), message))


def operations(doc):
    """Every (path, method, operation) triple in the document."""
    out = []
    for path, item in (doc.get("paths") or {}).items():
        for method, op in (item or {}).items():
            if method in HTTP_METHODS:
                out.append((path, method, op or {}))
    return out


def broken_refs(doc):
    """Local $refs that do not resolve. YAML parsing does not catch these."""
    broken = []

    def walk(node, path):
        if isinstance(node, dict):
            ref = node.get("$ref")
            if isinstance(ref, str) and ref.startswith("#/"):
                cur = doc
                for part in ref[2:].split("/"):
                    if isinstance(cur, dict) and part in cur:
                        cur = cur[part]
                    else:
                        broken.append("%s -> %s" % (path or "/", ref))
                        break
            for key, value in node.items():
                walk(value, "%s/%s" % (path, key))
        elif isinstance(node, list):
            for index, value in enumerate(node):
                walk(value, "%s[%d]" % (path, index))

    walk(doc, "")
    return broken


def referenced_schemas(doc):
    names = set()

    def walk(node):
        if isinstance(node, dict):
            ref = node.get("$ref")
            if isinstance(ref, str) and ref.startswith("#/components/schemas/"):
                names.add(ref.rsplit("/", 1)[1])
            for value in node.values():
                walk(value)
        elif isinstance(node, list):
            for value in node:
                walk(value)

    walk(doc)
    return names


def joined(items, limit=6):
    items = list(items)
    shown = ", ".join(str(i) for i in items[:limit])
    return shown + (", ..." if len(items) > limit else "")


def main(argv):
    if len(argv) != 2:
        print("no\tusage: validate-console-spec.py <spec.yaml>")
        return 1
    spec_path = argv[1]

    try:
        with open(spec_path, encoding="utf-8") as handle:
            doc = yaml.safe_load(handle)
    except FileNotFoundError:
        print("no\tfile not found: %s" % spec_path)
        return 1
    except yaml.YAMLError as exc:
        print("no\tdoes not parse: %s" % exc)
        return 1

    check(str(doc.get("openapi", "")).startswith("3."), "declares an OpenAPI 3.x version")

    # A mis-indented block becomes a sibling key under `paths` rather than an
    # error: a `responses:` block indented two spaces instead of six turned into
    # a path named "responses" and left the operation it belonged to with no
    # declared responses. Both conditions passed every other check here.
    non_paths = sorted(k for k in (doc.get("paths") or {}) if not str(k).startswith("/"))
    check(not non_paths, "every key under paths is a path (found: %s)" % joined(non_paths))

    no_responses = [
        "%s %s" % (method.upper(), path) for path, method, op in operations(doc)
        if "responses" not in op
    ]
    check(not no_responses, "every operation declares responses (missing: %s)" % joined(no_responses))

    # OpenAPI requires a description on every response object. 17 were missing,
    # which made the document invalid while every other check here passed.
    undescribed = [
        "%s %s %s" % (method.upper(), path, code)
        for path, method, op in operations(doc)
        for code, resp in (op.get("responses") or {}).items()
        if isinstance(resp, dict) and "$ref" not in resp and not resp.get("description")
    ]
    shared_undescribed = [
        name for name, resp in ((doc.get("components") or {}).get("responses") or {}).items()
        if isinstance(resp, dict) and not resp.get("description")
    ]
    check(
        not (undescribed or shared_undescribed),
        "every response declares a description (missing: %s)"
        % joined(undescribed + shared_undescribed),
    )

    ops = operations(doc)
    check(ops, "declares at least one operation")

    # Read-only contract. A write verb here means the scope boundary moved
    # without the separate design the plan requires.
    verbs = sorted({method for _, method, _ in ops})
    check(verbs == ["get"], "uses only GET (found: %s)" % joined(verbs))

    missing_id = [path for path, _, op in ops if not op.get("operationId")]
    check(not missing_id, "every operation has an operationId (missing: %s)" % joined(missing_id))

    ids = [op["operationId"] for _, _, op in ops if op.get("operationId")]
    dupe_ids = sorted({i for i in ids if ids.count(i) > 1})
    check(not dupe_ids, "operationIds are unique (duplicated: %s)" % joined(dupe_ids))

    # x-variants decides which adapters expose an operation; absent means a
    # generator has to guess, so it is required even when empty.
    missing_variants = [
        op.get("operationId", path) for path, _, op in ops if op.get("x-variants") is None
    ]
    check(
        not missing_variants,
        "every operation declares x-variants (missing: %s)" % joined(missing_variants),
    )

    unexpected = sorted(
        {v for _, _, op in ops for v in (op.get("x-variants") or []) if v not in KNOWN_VARIANTS}
    )
    check(not unexpected, "x-variants values are known (unexpected: %s)" % joined(unexpected))

    agent_ops = [op for _, _, op in ops if "agent" in (op.get("x-variants") or [])]
    no_tool = [
        op.get("operationId", "?") for op in agent_ops if not (op.get("x-mcp") or {}).get("tool")
    ]
    check(not no_tool, "every agent operation names an x-mcp tool (missing: %s)" % joined(no_tool))

    tools = [t for t in ((op.get("x-mcp") or {}).get("tool") for op in agent_ops) if t]
    dupe_tools = sorted({t for t in tools if tools.count(t) > 1})
    check(not dupe_tools, "x-mcp tool names are unique (duplicated: %s)" % joined(dupe_tools))

    # A tool name outside the agent variant would silently widen the agent
    # surface past what x-variants declares.
    stray = [
        op.get("operationId", "?")
        for _, _, op in ops
        if (op.get("x-mcp") or {}).get("tool") and "agent" not in (op.get("x-variants") or [])
    ]
    check(not stray, "no x-mcp tool outside the agent variant (found: %s)" % joined(stray))

    broken = broken_refs(doc)
    check(not broken, "all local $refs resolve (broken: %s)" % joined(broken, 4))

    schemas = set((doc.get("components") or {}).get("schemas") or {})
    orphans = sorted(schemas - referenced_schemas(doc))
    check(not orphans, "no unreferenced schemas (orphaned: %s)" % joined(orphans))

    for ok, message in results:
        print(("ok" if ok else "no") + "\t" + message)

    return 0 if all(ok for ok, _ in results) else 1


if __name__ == "__main__":
    sys.exit(main(sys.argv))
