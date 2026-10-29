#!/usr/bin/env bash
# =============================================================================
# Acceptance test for the storage-broker (docs/plans/presign-storage-broker.md).
#
# Runs the allow/deny matrix against a DEPLOYED broker, plus a real S3
# round-trip. Requires kubectl access to the cluster (the broker's ingress only
# admits tenant namespaces, so we port-forward — that path bypasses the
# NetworkPolicy, which is fine for a test driver).
#
# Identity:
#   - App token: minted with `kubectl create token` (audience storage-broker) —
#     this is exactly what an app pod would present.
#   - User token: a Zitadel access token. Pass it as USER_TOKEN to exercise the
#     user-scope cases; without it, app-scope + negative (401/403/400) cases run.
#
# Usage:
#   source scripts/env-from-keychain.sh   # materializes KUBECONFIG from Keychain
#   [USER_TOKEN="<zitadel access token>"] \
#   ./scripts/test-storage-broker.sh [tenant-namespace]      # default: ambrovia
# =============================================================================
set -euo pipefail

NS="${1:?Pass a tenant namespace from the selected deployment}"
BROKER_NS="${BROKER_NS:-infra}"
LOCAL_PORT="${LOCAL_PORT:-18080}"
BASE="http://127.0.0.1:${LOCAL_PORT}"
BODY=/tmp/sb_body.$$

pass=0
fail=0
ok() {
  echo "  PASS: $1"
  pass=$((pass + 1))
}
bad() {
  echo "  FAIL: $1"
  fail=$((fail + 1))
}

need() { command -v "$1" >/dev/null 2>&1 || {
  echo "missing dependency: $1" >&2
  exit 2
}; }
need kubectl
need curl
need jq

acceptance_uuid() {
  local seed="$1" digest
  digest="$(printf '%s' "${seed}-$(date +%s)-$$-${RANDOM}" | shasum -a 256 | awk '{print $1}')"
  printf '%s-%s-4%s-8%s-%s\n' \
    "${digest:0:8}" "${digest:8:4}" "${digest:13:3}" "${digest:17:3}" "${digest:20:12}"
}

echo "==> Minting app identity token (namespace=$NS, audience=storage-broker)"
APP_TOKEN="$(kubectl create token default -n "$NS" --audience storage-broker --duration 10m)"

echo "==> Port-forwarding svc/storage-broker -n $BROKER_NS"
kubectl -n "$BROKER_NS" port-forward svc/storage-broker "${LOCAL_PORT}:80" >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null || true; rm -f "$BODY"' EXIT
for _ in $(seq 1 40); do
  curl -sf "$BASE/healthz" >/dev/null 2>&1 && break
  sleep 0.25
done

# presign <app_token> <user_token> <json-body>  -> echoes HTTP code, body in $BODY
presign() {
  local at="$1" ut="$2" body="$3"
  local args=(-s -o "$BODY" -w '%{http_code}' -X POST "$BASE/v1/presign" -H 'Content-Type: application/json')
  [ -n "$at" ] && args+=(-H "Authorization: Bearer $at")
  [ -n "$ut" ] && args+=(-H "X-User-Authorization: Bearer $ut")
  args+=(-d "$body")
  curl "${args[@]}"
}

# Plain HTTP request to a presigned URL -> echoes HTTP code
transfer() { # <method> <url> [content-type] [data]
  local m="$1" url="$2" ct="${3:-}" data="${4:-}"
  local args=(-s -o /dev/null -w '%{http_code}' -X "$m")
  [ -n "$ct" ] && args+=(-H "Content-Type: $ct")
  [ -n "$data" ] && args+=(--data-binary "$data")
  curl "${args[@]}" "$url"
}

echo "==> Health"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/readyz")
[ "$code" = 200 ] && ok "readyz 200 (JWKS loaded)" || bad "readyz=$code (want 200)"

echo "==> Negative cases (no real tokens needed)"
code=$(presign "" "" '{"op":"get","key":"u/x/a"}')
[ "$code" = 401 ] && ok "missing app token -> 401" || bad "missing app token -> $code (want 401)"

code=$(presign "garbage.token.value" "" '{"op":"get","key":"u/x/a"}')
[ "$code" = 401 ] && ok "invalid app token -> 401" || bad "invalid app token -> $code (want 401)"

code=$(presign "$APP_TOKEN" "" '{"op":"get","key":"u/x/a"}')
[ "$code" = 403 ] && ok "valid app, no user token (require_user_token) -> 403" || bad "no user token -> $code (want 403)"

code=$(presign "$APP_TOKEN" "${USER_TOKEN:-}" '{"op":"get","key":"u/x/a","bucket":"nimmly-files"}')
[ "$code" = 400 ] && ok "stray bucket field -> 400 (DisallowUnknownFields)" || bad "stray bucket -> $code (want 400)"

# --- Service-scoped corpus/ path (credential-less crawler; NO user token) ----
# Only nimmly carries a placeholder-free `corpus/` prefix with require_user_token=false.
if [ "$NS" = nimmly ]; then
  echo "==> Service-scoped corpus/ cases (no user token — the crawler identity)"
  CKEY="corpus/acctest/$(date +%s)-$$.txt"

  code=$(presign "$APP_TOKEN" "" "{\"op\":\"put\",\"key\":\"$CKEY\",\"contentType\":\"text/plain\"}")
  if [ "$code" = 200 ]; then
    ok "corpus/ presign put (no user token) -> 200"
    PUT_URL="$(jq -r .url "$BODY")"
    t=$(transfer PUT "$PUT_URL" text/plain "crawler corpus acceptance $(date)")
    [ "$t" = 200 ] && ok "real S3 PUT to corpus/ -> 200" || bad "real S3 PUT to corpus/ -> $t (want 200)"

    code=$(presign "$APP_TOKEN" "" "{\"op\":\"get\",\"key\":\"$CKEY\"}")
    GET_URL="$(jq -r .url "$BODY")"
    t=$(transfer GET "$GET_URL")
    [ "$t" = 200 ] && ok "real S3 GET from corpus/ -> 200" || bad "real S3 GET from corpus/ -> $t (want 200)"

    code=$(presign "$APP_TOKEN" "" "{\"op\":\"delete\",\"key\":\"$CKEY\"}")
    DEL_URL="$(jq -r .url "$BODY")"
    transfer DELETE "$DEL_URL" >/dev/null 2>&1 || true # best-effort cleanup
  else
    bad "corpus/ presign put (no user token) -> $code (want 200)"
  fi

  # The decisive Task-C invariant: a user-scoped (${sub}) prefix WITHOUT a user
  # token must STILL be denied even though require_user_token=false — the ${sub}
  # placeholder, not the namespace bool, is what binds the key to a user.
  code=$(presign "$APP_TOKEN" "" '{"op":"get","key":"u/some-user/secret.txt"}')
  [ "$code" = 403 ] && ok "u/<sub>/ without user token -> 403 (per-user isolation preserved)" || bad "u/<sub>/ no token -> $code (want 403)"

  echo "==> Governed ordinary-object deletion"
  GOVERNED_TOKEN="$(kubectl create token transformation-storage-authority -n "$NS" --audience storage-broker --duration 15m)"
  WORKSPACE_A="${GOVERNED_WORKSPACE_A:-$(acceptance_uuid workspace-a)}"
  WORKSPACE_B="${GOVERNED_WORKSPACE_B:-$(acceptance_uuid workspace-b)}"
	CLEANUP_OWNER_A="${GOVERNED_CLEANUP_OWNER_A:-$(acceptance_uuid cleanup-owner-a)}"
	CLEANUP_OWNER_B="${GOVERNED_CLEANUP_OWNER_B:-$(acceptance_uuid cleanup-owner-b)}"
	CLEANUP_OWNER_C="${GOVERNED_CLEANUP_OWNER_C:-$(acceptance_uuid cleanup-owner-c)}"
	CLEANUP_OWNER_D="${GOVERNED_CLEANUP_OWNER_D:-$(acceptance_uuid cleanup-owner-d)}"
  GOVERNED_PAYLOAD="governed deletion acceptance $(date +%s)-$$"
  GOVERNED_HASH="$(printf '%s' "$GOVERNED_PAYLOAD" | shasum -a 256 | awk '{print $1}')"
  GOVERNED_SIZE="${#GOVERNED_PAYLOAD}"

  governed_reserve() { # <authority-json> [artifact-class] [cleanup-owner-id]
    local authority="$1" artifact_class="${2:-normalized_source}" cleanup_owner="${3:-$CLEANUP_OWNER_A}"
    local body
    body="$(jq -cn \
      --argjson authority "$authority" \
      --arg artifactClass "$artifact_class" \
			--arg cleanupOwnerId "$cleanup_owner" \
      --arg hash "$GOVERNED_HASH" \
      --argjson size "$GOVERNED_SIZE" \
      '{authority:$authority,artifactClass:$artifactClass,contentHash:$hash,byteSize:$size,mediaType:"text/plain",cleanupOwnerId:$cleanupOwnerId}')"
    curl -s -o "$BODY" -w '%{http_code}' -X POST "$BASE/v2/objects/reserve" \
      -H "Authorization: Bearer $GOVERNED_TOKEN" -H 'Content-Type: application/json' -d "$body"
  }

  governed_commit() { # <object-id>
    local object_id="$1"
    curl -s -o "$BODY" -w '%{http_code}' -X POST "$BASE/v2/objects/$object_id/commit" \
      -H "Authorization: Bearer $GOVERNED_TOKEN"
  }

  governed_delete() { # <object-id> <authority-json>
    local object_id="$1" authority="$2"
    local body
    body="$(jq -cn --argjson authority "$authority" '{authority:$authority}')"
    curl -s -o "$BODY" -w '%{http_code}' -X DELETE "$BASE/v2/objects/$object_id" \
      -H "Authorization: Bearer $GOVERNED_TOKEN" -H 'Content-Type: application/json' -d "$body"
  }

  governed_cleanup_reservations() { # <authority-json>
    local authority="$1"
    local body
    body="$(jq -cn --argjson authority "$authority" '{authority:$authority}')"
    curl -s -o "$BODY" -w '%{http_code}' -X DELETE "$BASE/v2/reservations" \
      -H "Authorization: Bearer $GOVERNED_TOKEN" -H 'Content-Type: application/json' -d "$body"
  }

	governed_cleanup_owner() { # <cleanup-owner-id> <authority-json>
		local cleanup_owner="$1" authority="$2" body
		body="$(jq -cn --argjson authority "$authority" '{authority:$authority}')"
		curl -s -o "$BODY" -w '%{http_code}' -X DELETE "$BASE/v2/cleanup-owners/$cleanup_owner/objects" \
			-H "Authorization: Bearer $GOVERNED_TOKEN" -H 'Content-Type: application/json' -d "$body"
	}

	governed_cleanup_workspace() { # <workspace-authority-json>
		local authority="$1" body
		body="$(jq -cn --argjson authority "$authority" '{authority:$authority}')"
		curl -s -o "$BODY" -w '%{http_code}' -X DELETE "$BASE/v2/ordinary-objects" \
			-H "Authorization: Bearer $GOVERNED_TOKEN" -H 'Content-Type: application/json' -d "$body"
	}

  WORKSPACE_AUTHORITY="$(jq -cn --arg workspace "$WORKSPACE_A" '{kind:"workspace",workspaceId:$workspace}')"
  OTHER_WORKSPACE_AUTHORITY="$(jq -cn --arg workspace "$WORKSPACE_B" '{kind:"workspace",workspaceId:$workspace}')"
  TENANT_AUTHORITY='{"kind":"tenant"}'

  workspace_object_id=""
  public_object_id=""
  abandoned_object_id=""
  other_workspace_object_id=""
  abandoned_put_url=""
	owner_object_id=""
	other_owner_object_id=""
  retry_after=0

  code=$(governed_reserve "$WORKSPACE_AUTHORITY")
  if [ "$code" = 201 ]; then
    workspace_object_id="$(jq -r .object.objectId "$BODY")"
    put_url="$(jq -r .transfer.url "$BODY")"
    t=$(transfer PUT "$put_url" text/plain "$GOVERNED_PAYLOAD")
    [ "$t" = 200 ] && ok "upload governed workspace staging bytes -> 200" || bad "workspace staging upload -> $t (want 200)"
    code=$(governed_commit "$workspace_object_id")
    if [ "$code" = 201 ]; then
      ok "commit ordinary workspace object -> 201"
      code=$(governed_delete "$workspace_object_id" "$OTHER_WORKSPACE_AUTHORITY")
      [ "$code" = 403 ] && ok "other workspace delete -> 403" || bad "other workspace delete -> $code (want 403)"
      code=$(governed_delete "$workspace_object_id" "$WORKSPACE_AUTHORITY")
      if [ "$code" = 202 ] && [ "$(jq -r .accessState "$BODY")" = revoked ] &&
        [ "$(jq -r .physicalRemovalState "$BODY")" = pending ]; then
        retry_after="$(jq -r .retryAfterSeconds "$BODY")"
        ok "exact workspace delete revokes access before physical removal -> 202"
      else
        bad "initial workspace delete -> $code $(cat "$BODY")"
      fi
    else
      bad "commit ordinary workspace object -> $code (want 201)"
    fi
  else
    bad "reserve ordinary workspace object -> $code (want 201)"
  fi

  code=$(governed_reserve "$TENANT_AUTHORITY")
  if [ "$code" = 201 ]; then
    public_object_id="$(jq -r .object.objectId "$BODY")"
    put_url="$(jq -r .transfer.url "$BODY")"
    t=$(transfer PUT "$put_url" text/plain "$GOVERNED_PAYLOAD")
    [ "$t" = 200 ] && ok "upload governed public staging bytes -> 200" || bad "public staging upload -> $t (want 200)"
    code=$(governed_commit "$public_object_id")
    if [ "$code" = 201 ]; then
      ok "commit ordinary public object -> 201"
      code=$(governed_delete "$public_object_id" "$WORKSPACE_AUTHORITY")
      [ "$code" = 403 ] && ok "workspace delete cannot affect public object -> 403" || bad "workspace/public delete -> $code (want 403)"
      code=$(governed_delete "$public_object_id" "$TENANT_AUTHORITY")
      if [ "$code" = 202 ]; then
        public_retry="$(jq -r .retryAfterSeconds "$BODY")"
        [ "$public_retry" -gt "$retry_after" ] && retry_after="$public_retry"
        ok "exact tenant-public delete revokes access before physical removal -> 202"
      else
        bad "initial tenant-public delete -> $code $(cat "$BODY")"
      fi
    else
      bad "commit ordinary public object -> $code (want 201)"
    fi
  else
    bad "reserve ordinary public object -> $code (want 201)"
  fi

  code=$(governed_reserve "$WORKSPACE_AUTHORITY" analysis_manifest)
  if [ "$code" = 201 ]; then
    abandoned_object_id="$(jq -r .object.objectId "$BODY")"
    abandoned_put_url="$(jq -r .transfer.url "$BODY")"
    t=$(transfer PUT "$abandoned_put_url" text/plain "$GOVERNED_PAYLOAD")
    [ "$t" = 200 ] && ok "upload analysis manifest before lost-response cleanup -> 200" || bad "analysis manifest upload -> $t (want 200)"
  else
    bad "reserve analysis manifest -> $code (want 201)"
  fi

  code=$(governed_reserve "$OTHER_WORKSPACE_AUTHORITY" analysis_result "$CLEANUP_OWNER_B")
  if [ "$code" = 201 ]; then
    other_workspace_object_id="$(jq -r .object.objectId "$BODY")"
    other_put_url="$(jq -r .transfer.url "$BODY")"
    t=$(transfer PUT "$other_put_url" text/plain "$GOVERNED_PAYLOAD")
    [ "$t" = 200 ] && ok "upload other-workspace analysis result -> 200" || bad "analysis result upload -> $t (want 200)"
  else
    bad "reserve analysis result -> $code (want 201)"
  fi

	for owner in "$CLEANUP_OWNER_C" "$CLEANUP_OWNER_D"; do
		code=$(governed_reserve "$WORKSPACE_AUTHORITY" analysis_result "$owner")
		if [ "$code" = 201 ]; then
			object_id="$(jq -r .object.objectId "$BODY")"
			put_url="$(jq -r .transfer.url "$BODY")"
			t=$(transfer PUT "$put_url" text/plain "$GOVERNED_PAYLOAD")
			code=$(governed_commit "$object_id")
			if [ "$t" = 200 ] && [ "$code" = 201 ]; then
				[ "$owner" = "$CLEANUP_OWNER_C" ] && owner_object_id="$object_id" || other_owner_object_id="$object_id"
				ok "prepare cleanup-owner ordinary object -> active"
			else
				bad "prepare cleanup-owner object -> PUT $t commit $code"
			fi
		else
			bad "reserve cleanup-owner object -> $code"
		fi
	done
	if [ -n "$owner_object_id" ] && [ -n "$other_owner_object_id" ]; then
		code=$(governed_cleanup_owner "$CLEANUP_OWNER_C" "$WORKSPACE_AUTHORITY")
		if [ "$code" = 202 ] && [ "$(jq -r .pendingObjectCount "$BODY")" -ge 1 ]; then
			owner_retry="$(jq -r .retryAfterSeconds "$BODY")"
			[ "$owner_retry" -gt "$retry_after" ] && retry_after="$owner_retry"
			ok "cleanup owner revokes only its active objects -> 202"
		else
			bad "initial cleanup-owner delete -> $code $(cat "$BODY")"
		fi
		code=$(governed_commit "$other_owner_object_id")
		[ "$code" = 201 ] && ok "other cleanup owner remains active" || bad "owner cleanup crossed owner boundary -> $code"
		code=$(governed_cleanup_workspace "$WORKSPACE_AUTHORITY")
		if [ "$code" = 202 ]; then
			workspace_retry="$(jq -r .retryAfterSeconds "$BODY")"
			[ "$workspace_retry" -gt "$retry_after" ] && retry_after="$workspace_retry"
			ok "whole-workspace ordinary cleanup revokes remaining owners -> 202"
		else
			bad "initial whole-workspace cleanup -> $code $(cat "$BODY")"
		fi
	fi

  if [ -n "$abandoned_object_id" ] && [ -n "$other_workspace_object_id" ]; then
    code=$(governed_cleanup_reservations "$WORKSPACE_AUTHORITY")
    if [ "$code" = 202 ] && [ "$(jq -r .accessState "$BODY")" = revoked ] &&
      [ "$(jq -r .physicalRemovalState "$BODY")" = pending ]; then
      reservation_retry="$(jq -r .retryAfterSeconds "$BODY")"
      [ "$reservation_retry" -gt "$retry_after" ] && retry_after="$reservation_retry"
      ok "lost-response reservation cleanup revokes access -> 202"
    else
      bad "initial reservation cleanup -> $code $(cat "$BODY")"
    fi
    code=$(governed_commit "$abandoned_object_id")
    [ "$code" = 403 ] && ok "late commit after reservation cleanup -> 403" || bad "late abandoned commit -> $code (want 403)"
    code=$(governed_commit "$other_workspace_object_id")
    if [ "$code" = 201 ]; then
      ok "reservation cleanup leaves other workspace analysis result active -> 201"
      code=$(governed_delete "$other_workspace_object_id" "$OTHER_WORKSPACE_AUTHORITY")
      if [ "$code" = 202 ]; then
        other_retry="$(jq -r .retryAfterSeconds "$BODY")"
        [ "$other_retry" -gt "$retry_after" ] && retry_after="$other_retry"
        ok "analysis result deletion revokes access -> 202"
      else
        bad "analysis result deletion -> $code $(cat "$BODY")"
      fi
    else
      bad "other workspace analysis result commit -> $code (want 201)"
    fi
  fi

  if [ -n "$workspace_object_id" ] && [ -n "$public_object_id" ] &&
    [ -n "$abandoned_object_id" ] && [ -n "$other_workspace_object_id" ] && [ "$retry_after" -gt 0 ]; then
    echo "    waiting ${retry_after}s for previously issued PUT tickets to expire"
    sleep "$((retry_after + 1))"
    code=$(governed_delete "$workspace_object_id" "$WORKSPACE_AUTHORITY")
    if [ "$code" = 200 ] && [ "$(jq -r .physicalRemovalState "$BODY")" = confirmed ]; then
      ok "workspace physical removal confirmed after ticket expiry -> 200"
    else
      bad "workspace physical removal retry -> $code $(cat "$BODY")"
    fi
    code=$(governed_delete "$workspace_object_id" "$WORKSPACE_AUTHORITY")
    [ "$code" = 200 ] && ok "completed workspace delete retry -> 200" || bad "completed workspace delete retry -> $code (want 200)"
    code=$(governed_delete "$public_object_id" "$TENANT_AUTHORITY")
    if [ "$code" = 200 ] && [ "$(jq -r .physicalRemovalState "$BODY")" = confirmed ]; then
      ok "tenant-public physical cleanup confirmed -> 200"
    else
      bad "tenant-public physical cleanup retry -> $code $(cat "$BODY")"
    fi
    code=$(governed_cleanup_reservations "$WORKSPACE_AUTHORITY")
    if [ "$code" = 200 ] && [ "$(jq -r .physicalRemovalState "$BODY")" = confirmed ] &&
      [ "$(jq -r .pendingReservationCount "$BODY")" = 0 ]; then
      ok "abandoned reservation physical cleanup confirmed -> 200"
    else
      bad "abandoned reservation cleanup retry -> $code $(cat "$BODY")"
    fi
    t=$(transfer PUT "$abandoned_put_url" text/plain "$GOVERNED_PAYLOAD")
    [ "$t" != 200 ] && ok "expired upload ticket cannot recreate bytes after cleanup -> $t" || bad "late upload recreated bytes after confirmed cleanup"
    code=$(governed_cleanup_reservations "$WORKSPACE_AUTHORITY")
    [ "$code" = 200 ] && ok "completed reservation cleanup retry -> 200" || bad "completed reservation cleanup retry -> $code (want 200)"
    code=$(governed_delete "$other_workspace_object_id" "$OTHER_WORKSPACE_AUTHORITY")
    if [ "$code" = 200 ] && [ "$(jq -r .physicalRemovalState "$BODY")" = confirmed ]; then
      ok "analysis result physical cleanup confirmed -> 200"
    else
      bad "analysis result cleanup retry -> $code $(cat "$BODY")"
    fi
		if [ -n "$owner_object_id" ] && [ -n "$other_owner_object_id" ]; then
			code=$(governed_cleanup_owner "$CLEANUP_OWNER_C" "$WORKSPACE_AUTHORITY")
			[ "$code" = 200 ] && [ "$(jq -r .pendingObjectCount "$BODY")" = 0 ] &&
				ok "cleanup-owner physical removal confirmed -> 200" || bad "cleanup-owner retry -> $code $(cat "$BODY")"
			code=$(governed_cleanup_workspace "$WORKSPACE_AUTHORITY")
			[ "$code" = 200 ] && [ "$(jq -r .pendingObjectCount "$BODY")" = 0 ] &&
				ok "whole-workspace physical removal confirmed -> 200" || bad "workspace cleanup retry -> $code $(cat "$BODY")"
		fi
  fi
fi

if [ -n "${USER_TOKEN:-}" ]; then
  echo "==> User-scope cases"
  # Derive sub via userinfo (the token is opaque/JWE — not locally decodable),
  # exactly as the broker validates it.
  SUB="$(curl -s --max-time 10 -H "Authorization: Bearer $USER_TOKEN" "${OIDC_ISSUER:?Set OIDC_ISSUER for the selected deployment}/oidc/v1/userinfo" | jq -r '.sub // empty')"
  if [ -z "$SUB" ] || [ "$SUB" = null ]; then
    bad "could not decode sub from USER_TOKEN"
  else
    echo "    (sub=$SUB)"
    KEY="u/${SUB}/acctest-$(date +%s).txt"

    code=$(presign "$APP_TOKEN" "$USER_TOKEN" "{\"op\":\"put\",\"key\":\"$KEY\",\"contentType\":\"text/plain\"}")
    if [ "$code" = 200 ]; then
      ok "own-prefix presign put -> 200"
      PUT_URL="$(jq -r .url "$BODY")"
      t=$(transfer PUT "$PUT_URL" text/plain "storage-broker acceptance $(date)")
      [ "$t" = 200 ] && ok "real S3 PUT -> 200" || bad "real S3 PUT -> $t (want 200)"

      code=$(presign "$APP_TOKEN" "$USER_TOKEN" "{\"op\":\"get\",\"key\":\"$KEY\"}")
      GET_URL="$(jq -r .url "$BODY")"
      t=$(transfer GET "$GET_URL")
      [ "$t" = 200 ] && ok "real S3 GET -> 200" || bad "real S3 GET -> $t (want 200)"

      code=$(presign "$APP_TOKEN" "$USER_TOKEN" "{\"op\":\"delete\",\"key\":\"$KEY\"}")
      DEL_URL="$(jq -r .url "$BODY")"
      t=$(transfer DELETE "$DEL_URL")
      [ "$t" = 204 ] && ok "real S3 DELETE (cleanup) -> 204" || bad "real S3 DELETE -> $t (want 204)"
    else
      bad "own-prefix presign put -> $code (want 200)"
    fi

    code=$(presign "$APP_TOKEN" "$USER_TOKEN" '{"op":"get","key":"u/some-other-user/secret.txt"}')
    [ "$code" = 403 ] && ok "another user's prefix -> 403" || bad "other user's prefix -> $code (want 403)"

    code=$(presign "$APP_TOKEN" "$USER_TOKEN" '{"op":"get","key":"../../etc/passwd"}')
    [ "$code" = 403 ] && ok "path traversal -> 403" || bad "path traversal -> $code (want 403)"
  fi
else
  echo "==> USER_TOKEN not set — skipping user-scope allow cases"
fi

echo "--------------------------------------------------"
echo "RESULT: $pass passed, $fail failed"
[ "$fail" = 0 ]
