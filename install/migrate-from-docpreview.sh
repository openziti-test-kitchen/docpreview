#!/usr/bin/env bash
#
# Move a host installed from the docpreview rpm or deb (v0.4.9 and earlier) to nfpreview, and back.
#
# PR #23 renamed everything and reads none of the old names, so a package upgrade alone does not
# carry an installation across. This does it in place, on the host, as root:
#
#   migrate-from-docpreview.sh check      read-only: what is installed, what would move, what is live
#   migrate-from-docpreview.sh stage      copy the docker build caches to their new names (old daemon keeps running)
#   migrate-from-docpreview.sh cutover    stop docpreview, move everything, install nfpreview, start it
#   migrate-from-docpreview.sh verify     read-only: is nfpreview up, and does every public URL answer
#   migrate-from-docpreview.sh rollback   undo cutover and start docpreview v0.4.9 again
#   migrate-from-docpreview.sh finish     after the soak: remove the docpreview package and old caches
#
# The runbook is www/docs/guides/migrate-from-docpreview.md. tools/migrate-docprev.sh drives this
# from a workstation over ssh.
#
# What cutover moves, and why each one:
#
#   the service account   renamed in place (usermod -l), so the uid and every file's owner stay put
#   /var/lib/docpreview   moved to /var/lib/nfpreview: database, vault, artifacts, logs, zrok env
#   /etc/docpreview       moved to /etc/nfpreview: config.yml and the master key
#   docpreview.db         renamed to nfpreview.db, and each preview's stored artifact_dir rewritten.
#                         Without the rewrite the new daemon finds no artifacts at the old paths and
#                         deletes every preview row at startup.
#   share-gate files      renamed, so the zrok create budget carries over instead of resetting
#   config.yml            data_dir, log_file and key_source paths rewritten. dashboard_url is not.
#   build caches          docpreview-cache-* copied to nfpreview-cache-* (stage), or left cold
#   the units             nfpreview's, plus drop-ins that keep the old public names (WEBHOOK_NAME,
#                         DASH_NAME) so the GitHub App, the Bitbucket webhooks and the dashboard
#                         links in old comments keep working
#
# The old zrok shares carry the tag docpreview:<preview>. OLD_SHARES says what happens to them:
#
#   adopt    leave them. Only for an nfpreview build that recognises the old tag. Previews stay
#            up through the cutover and nothing is created.
#   delete   delete them while the daemon is stopped, so nfpreview can create its own under the
#            same reserved names. For a build that does not recognise the old tag. One gated
#            create per preview at startup, and the preview URLs are down until each is done.
#
# Nothing is deleted from disk. The docpreview package stays installed, stopped, until finish, so
# rollback needs no download.
set -Eeuo pipefail

OLD=docpreview
NEW=nfpreview
OLD_DATA=/var/lib/$OLD
NEW_DATA=/var/lib/$NEW
OLD_ETC=/etc/$OLD
NEW_ETC=/etc/$NEW
UNITS_DIR=/etc/systemd/system

STAGE="${STAGE:-/var/tmp/nfpreview-migration}"
OLD_RPM="${OLD_RPM:-/var/tmp/docpreview-0.4.9-1.x86_64.rpm}"
NEW_RPM="${NEW_RPM:-}"
# Empty means "use the packaged nfpreview / nfpreview-dash names", which needs the outside
# changes listed in the runbook. The defaults keep the URLs production already publishes.
WEBHOOK_NAME="${WEBHOOK_NAME-docpreview}"
DASH_NAME="${DASH_NAME-docpreview-dash}"
OLD_SHARES="${OLD_SHARES:-}"
COPY_CACHES="${COPY_CACHES:-1}"
ENABLE_AT_BOOT="${ENABLE_AT_BOOT:-0}"
HEALTH_WAIT="${HEALTH_WAIT:-180}"

log() { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*"; }
step() { printf '\n%s  == %s\n' "$(date -u +%H:%M:%S)" "$*"; CURRENT_STEP="$*"; }
warn() { printf '%s  WARNING: %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }
die() { printf '%s  STOP: %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run as root (sudo)"
command -v python3 >/dev/null || die "python3 is needed for the database steps"

user_exists() { getent passwd "$1" >/dev/null 2>&1; }
pkg_installed() { rpm -q "$1" >/dev/null 2>&1; }
units_of() { echo "$1-webhook $1-dashboard $1"; }

# The data directory that is current right now, whichever name it has.
current_data() {
  if [ -d "$NEW_DATA/zrok2" ] || [ -f "$NEW_DATA/vault.age" ]; then echo "$NEW_DATA"; else echo "$OLD_DATA"; fi
}
current_user() { if user_exists "$NEW"; then echo "$NEW"; else echo "$OLD"; fi; }

new_rpm() {
  if [ -n "$NEW_RPM" ]; then echo "$NEW_RPM"; return; fi
  local found=() f
  for f in "$STAGE"/nfpreview-*.x86_64.rpm; do [ -f "$f" ] && found+=("$f"); done
  [ "${#found[@]}" = 1 ] || die "expected exactly one nfpreview rpm in $STAGE, found ${#found[@]}; set NEW_RPM"
  echo "${found[0]}"
}

# Checked against the SHA256SUMS staged beside it, when there is one.
verify_rpm() {
  local rpm="$1" sums
  [ -s "$rpm" ] || die "missing or empty: $rpm"
  sums="$(dirname "$rpm")/SHA256SUMS"
  if [ -f "$sums" ]; then
    (cd "$(dirname "$rpm")" && grep -E "[ *]$(basename "$rpm")\$" SHA256SUMS | sha256sum -c --quiet -) ||
      die "$rpm does not match $sums"
    log "sha256 ok: $(basename "$rpm")"
  else
    log "sha256 $(sha256sum "$rpm" | cut -d' ' -f1)  $(basename "$rpm")  (no SHA256SUMS to check against)"
  fi
}

# ── zrok ──────────────────────────────────────────────────────────────────────────────────────
#
# The zrok2 CLI reads $HOME/.zrok2 and has no flag for another directory, so it runs as the service
# account with a throwaway HOME whose .zrok2 points at the installation's own environment. That is
# the environment that owns the preview shares; listing is filtered to it as well, because the
# account holds other environments' shares too.
zrok_in() {
  local user="$1" data="$2"; shift 2
  local home
  home="$(mktemp -d /var/tmp/nfpreview-zrokhome.XXXXXX)"
  chown "$user": "$home"
  ln -s "$data/zrok2" "$home/.zrok2"
  local rc=0
  runuser -u "$user" -- env HOME="$home" zrok2 "$@" || rc=$?
  rm -rf "$home"
  return $rc
}

# Prints "token target endpoint" for each share in this environment whose target starts with $3.
tagged_shares() {
  local user="$1" data="$2" prefix="$3" json
  json="$(zrok_in "$user" "$data" list shares --json --target "$prefix")" || die "zrok2 list shares failed"
  ENVJSON="$data/zrok2/environment.json" PREFIX="$prefix" python3 -c '
import json, os, sys
env = json.load(open(os.environ["ENVJSON"]))["ziti_identity"]
prefix = os.environ["PREFIX"]
for s in json.loads(sys.stdin.read() or "{}").get("shares") or []:
    if s.get("envZId") == env and (s.get("target") or "").startswith(prefix):
        print(s["shareToken"], s["target"], (s.get("frontendEndpoints") or ["-"])[0])
' <<<"$json"
}

delete_tagged_shares() {
  local user="$1" data="$2" prefix="$3" failed=0 token target endpoint n rc
  while read -r token target endpoint; do
    [ -n "$token" ] || continue
    for n in 1 2 3 4 5; do
      rc=0
      zrok_in "$user" "$data" delete share "$token" >/dev/null 2>&1 || rc=$?
      # 1 is transient (unreachable, 429, 5xx) and worth another try; 2 is a refusal.
      [ "$rc" = 1 ] || break
      sleep $((n * 10))
    done
    if [ "$rc" = 0 ]; then log "deleted share $token ($target, $endpoint)"
    else warn "could not delete share $token ($target, $endpoint), zrok2 exit $rc"; failed=1; fi
    sleep 2
  done < <(tagged_shares "$user" "$data" "$prefix")
  [ "$failed" = 0 ] || die "some $prefix shares were not deleted; see above"
}

# ── the database ──────────────────────────────────────────────────────────────────────────────
#
# Run as the service account so the -wal and -shm files sqlite creates keep the right owner. The
# checkpoint folds the WAL into the main file first, so renaming one file moves all of the state.
rewrite_db() {
  local user="$1" db="$2" from="$3" to="$4"
  runuser -u "$user" -- python3 - "$db" "$from" "$to" <<'PY'
import sqlite3, sys
db, src, dst = sys.argv[1:4]
c = sqlite3.connect(db)
c.execute("PRAGMA busy_timeout=5000")
c.execute("PRAGMA wal_checkpoint(TRUNCATE)")
n = c.execute(
    "UPDATE previews SET artifact_dir = ? || substr(artifact_dir, ?) WHERE artifact_dir LIKE ? || '%'",
    (dst, len(src) + 1, src)).rowcount
c.commit()
c.execute("PRAGMA wal_checkpoint(TRUNCATE)")
left = c.execute("SELECT count(*) FROM previews WHERE artifact_dir LIKE ? || '%'", (src,)).fetchone()[0]
c.close()
print(f"artifact_dir rewritten on {n} preview(s), {left} still under {src}")
sys.exit(1 if left else 0)
PY
}

# Moves name.db and any -wal/-shm beside it together. The checkpoint above leaves none, normally.
rename_db() {
  local dir="$1" from="$2" to="$3" suffix
  [ -f "$dir/$from.db" ] || return 0
  [ ! -e "$dir/$to.db" ] || die "$dir/$to.db already exists; refusing to overwrite it"
  for suffix in .db .db-wal .db-shm; do
    if [ -e "$dir/$from$suffix" ]; then mv "$dir/$from$suffix" "$dir/$to$suffix"; fi
  done
  log "database: $dir/$from.db -> $to.db"
}

rename_gate_files() {
  local dir="$1/zrok2" from="$2" to="$3" f
  for f in share-gate.json share-gate.json.lock shares.jsonl; do
    if [ -e "$dir/$from-$f" ] && [ ! -e "$dir/$to-$f" ]; then
      mv "$dir/$from-$f" "$dir/$to-$f"
      log "share gate: $from-$f -> $to-$f"
    fi
  done
}

# Only the three absolute paths. Comments and dashboard_url are left as they are.
rewrite_config() {
  local cfg="$1" from="$2" to="$3"
  [ -f "$cfg" ] || die "no config at $cfg"
  sed -i -E \
    -e "s#^([[:space:]]*data_dir:[[:space:]]*\"?)/var/lib/$from#\1/var/lib/$to#" \
    -e "s#^([[:space:]]*log_file:[[:space:]]*\"?)/var/lib/$from/#\1/var/lib/$to/#" \
    -e "s#^([[:space:]]*key_source:[[:space:]]*\"?file:)/etc/$from/#\1/etc/$to/#" \
    "$cfg"
  log "config: paths now"
  grep -E '^[[:space:]]*(data_dir|log_file|dashboard_url):' "$cfg" | sed 's/^/           /'
  grep -E '^[[:space:]]*key_source:[[:space:]]*"?file:' "$cfg" | sed 's/^/           /' || true
}

# ── units ─────────────────────────────────────────────────────────────────────────────────────

stop_units() {
  local u
  for u in $(units_of "$1"); do
    if systemctl list-unit-files "$u.service" >/dev/null 2>&1 && systemctl is-active --quiet "$u"; then
      systemctl stop "$u"
      log "stopped $u"
    fi
  done
}

wait_health() {
  local url="http://127.0.0.1:8471/healthz" i code
  for ((i = 0; i < HEALTH_WAIT; i += 3)); do
    code="$(curl -s -o /dev/null -m 3 -w '%{http_code}' "$url" || true)"
    [ "$code" = 200 ] && { log "daemon answers $url"; return 0; }
    sleep 3
  done
  return 1
}

start_units() {
  local name="$1" u
  systemctl daemon-reload
  systemctl start "$name"
  wait_health || die "$name did not answer within ${HEALTH_WAIT}s: journalctl -u $name -n 100"
  systemctl start "$name-webhook" "$name-dashboard"
  sleep 5
  for u in $(units_of "$name"); do
    systemctl is-active --quiet "$u" || die "$u is not active: journalctl -u $u -n 50"
    log "$u: $(systemctl is-active "$u")"
  done
  if [ "$ENABLE_AT_BOOT" = 1 ]; then
    systemctl enable $(units_of "$name") >/dev/null 2>&1
    log "enabled at boot: $(units_of "$name")"
  fi
}

# Drop-ins that keep a public name. ExecStart is replaced whole, because an argument cannot be
# overridden on its own.
write_dropins() {
  local d
  rm -rf "$UNITS_DIR/$NEW-webhook.service.d/10-keep-name.conf" "$UNITS_DIR/$NEW-dashboard.service.d/10-keep-name.conf"
  if [ -n "$WEBHOOK_NAME" ]; then
    d="$UNITS_DIR/$NEW-webhook.service.d"; mkdir -p "$d"
    cat >"$d/10-keep-name.conf" <<EOF
# Written by migrate-from-docpreview.sh: keep the webhook URL the GitHub App and the Bitbucket
# repositories already deliver to, https://$WEBHOOK_NAME.shares.zrok.io/webhook/...
[Service]
ExecStart=
ExecStart=/usr/local/bin/$NEW webhook-only -zrok-name $WEBHOOK_NAME -zrok-home $NEW_DATA/zrok2 -path /webhook/github -path /webhook/bitbucket
EOF
    log "drop-in: $NEW-webhook keeps -zrok-name $WEBHOOK_NAME"
  fi
  if [ -n "$DASH_NAME" ]; then
    d="$UNITS_DIR/$NEW-dashboard.service.d"; mkdir -p "$d"
    cat >"$d/10-keep-name.conf" <<EOF
# Written by migrate-from-docpreview.sh: keep the dashboard URL that config.yml's dashboard_url and
# every existing pull request comment link to, https://$DASH_NAME.shares.zrok.io/
[Service]
ExecStart=
ExecStart=/usr/local/bin/$NEW dashboard-only -zrok-name $DASH_NAME -zrok-home $NEW_DATA/zrok2
EOF
    log "drop-in: $NEW-dashboard keeps -zrok-name $DASH_NAME"
  fi
  systemctl daemon-reload
}

# ── docker ────────────────────────────────────────────────────────────────────────────────────

running_builds() {
  local id src
  for id in $(docker ps -q 2>/dev/null); do
    for src in $(docker inspect -f '{{range .Mounts}}{{.Source}} {{end}}' "$id"); do
      case "$src" in "$OLD_DATA"/* | "$NEW_DATA"/*) echo "$id"; break ;; esac
    done
  done
}

copy_caches() {
  local v new copied=0
  command -v docker >/dev/null || { warn "no docker; caches not copied"; return 0; }
  docker image inspect alpine:3 >/dev/null 2>&1 || die "alpine:3 is not present locally; COPY_CACHES=0 to skip"
  for v in $(docker volume ls -q --filter "name=$OLD-cache-"); do
    case "$v" in "$OLD-cache-"*) ;; *) continue ;; esac
    new="$NEW-cache-${v#"$OLD-cache-"}"
    if docker volume inspect "$new" >/dev/null 2>&1; then continue; fi
    docker volume create "$new" >/dev/null
    docker run --rm --network none -v "$v":/from:ro -v "$new":/to alpine:3 cp -a /from/. /to/
    copied=$((copied + 1))
    log "cache: $v -> $new"
  done
  log "caches copied: $copied"
}

# ── subcommands ───────────────────────────────────────────────────────────────────────────────

cmd_check() {
  local data user cfg
  data="$(current_data)"; user="$(current_user)"
  if [ "$data" = "$NEW_DATA" ]; then cfg="$NEW_ETC/config.yml"; else cfg="$OLD_ETC/config.yml"; fi

  step "packages and binaries"
  rpm -q "$OLD" "$NEW" || true
  for b in /usr/local/bin/$OLD /usr/local/bin/$NEW; do
    if [ -x "$b" ]; then log "$b: $("$b" version 2>&1 | head -1)"; fi
  done
  if [ -f "$OLD_RPM" ]; then log "rollback rpm: $OLD_RPM $(sha256sum "$OLD_RPM" | cut -d' ' -f1)"; else warn "no rollback rpm at $OLD_RPM"; fi
  if [ -n "$NEW_RPM" ] || compgen -G "$STAGE/nfpreview-*.x86_64.rpm" >/dev/null; then verify_rpm "$(new_rpm)"; fi

  step "units"
  for u in $(units_of "$OLD") $(units_of "$NEW"); do
    printf '           %-22s active=%-9s enabled=%s\n' "$u" "$(systemctl is-active "$u" 2>/dev/null || true)" \
      "$(systemctl is-enabled "$u" 2>/dev/null || true)"
  done
  ls -d "$UNITS_DIR"/*preview*.service.d 2>/dev/null || log "no drop-ins"

  step "account and directories"
  getent passwd "$OLD" "$NEW" || true
  ls -ld "$OLD_DATA" "$NEW_DATA" "$OLD_ETC" "$NEW_ETC" 2>&1 | sed 's/^/           /'
  df -h "$data" | sed 's/^/           /'

  step "config ($cfg, paths only)"
  grep -E '^[[:space:]]*(listen|data_dir|log_file|dashboard_url):' "$cfg" | sed 's/^/           /'
  grep -E '^[[:space:]]*key_source:[[:space:]]*"?file:' "$cfg" | sed 's/^/           /' || log "key_source is not a file: source"

  step "database (read-only)"
  local db="$data/$NEW.db"; [ -f "$db" ] || db="$data/$OLD.db"
  python3 - "$db" <<'PY'
import sqlite3, sys
c = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
q = lambda s: c.execute(s).fetchall()
print("           file:", sys.argv[1])
print("           previews by state:", dict(q("SELECT state, count(*) FROM previews GROUP BY state")))
roots = {}
for (a,) in q("SELECT artifact_dir FROM previews WHERE artifact_dir != ''"):
    r = "/".join(a.split("/")[:4])
    roots[r] = roots.get(r, 0) + 1
print("           artifact_dir roots:", roots)
print("           queued jobs:", q("SELECT count(*) FROM jobs")[0][0])
print("           projects:", ", ".join(f"{p}:{o}/{r}" for p, o, r in q("SELECT platform, owner, repo FROM projects ORDER BY 1, 2, 3")))
for k, v in q("SELECT key, value FROM settings WHERE key LIKE 'exposer.%' ORDER BY key"):
    print(f"           setting {k} = {v}")
print("           other settings (names only):", ", ".join(k for (k,) in q("SELECT key FROM settings WHERE key NOT LIKE 'exposer.%' ORDER BY key")))
PY

  step "zrok shares in this installation's environment"
  if command -v zrok2 >/dev/null && [ -d "$data/zrok2" ]; then
    for prefix in "$OLD:" "$NEW:"; do
      log "tagged $prefix"
      tagged_shares "$user" "$data" "$prefix" | sed 's/^/           /'
    done
  else
    warn "no zrok2 CLI or no $data/zrok2; shares not listed"
  fi

  step "docker"
  log "running builds: $(running_builds | wc -l)"
  log "cache volumes: $OLD=$(docker volume ls -q --filter "name=$OLD-cache-" | wc -l) $NEW=$(docker volume ls -q --filter "name=$NEW-cache-" | wc -l)"
}

cmd_stage() {
  pkg_installed "$OLD" || die "$OLD is not installed; nothing to stage"
  verify_rpm "$(new_rpm)"
  if [ -f "$OLD_RPM" ]; then verify_rpm "$OLD_RPM"; else warn "no rollback rpm at $OLD_RPM; rollback after finish will need one"; fi
  if [ "$COPY_CACHES" = 1 ]; then
    step "copy build caches (docpreview keeps running)"
    copy_caches
  fi
}

on_cutover_error() {
  printf '\n%s  CUTOVER STOPPED during: %s\n' "$(date -u +%H:%M:%S)" "${CURRENT_STEP:-?}" >&2
  printf '           Nothing is started. Fix and run cutover again (each step skips what is done),\n' >&2
  printf '           or run rollback to return to %s.\n' "$OLD" >&2
}

cmd_cutover() {
  local rpm ts backup
  case "$OLD_SHARES" in
    adopt | delete) ;;
    *) die "set OLD_SHARES=adopt (nfpreview build recognises docpreview: shares) or OLD_SHARES=delete" ;;
  esac
  trap on_cutover_error ERR

  step "preflight"
  rpm="$(new_rpm)"; verify_rpm "$rpm"
  [ -f "$OLD_RPM" ] || warn "no rollback rpm at $OLD_RPM (only needed for a rollback after finish)"
  pkg_installed "$OLD" || pkg_installed "$NEW" || die "neither $OLD nor $NEW is installed"
  command -v zrok2 >/dev/null || [ "$OLD_SHARES" = adopt ] || die "OLD_SHARES=delete needs the zrok2 CLI"
  ts="$(date -u +%Y%m%dT%H%M%SZ)"
  backup="$STAGE/backup-$ts"
  install -d -m 0700 "$backup"
  log "cutover started $(date -u '+%Y-%m-%d %H:%M:%S UTC'); webhooks are not answered from here until the start step"

  step "stop $OLD"
  stop_units "$OLD"
  local i
  for i in $(seq 1 15); do pgrep -u "$OLD" >/dev/null 2>&1 || break; sleep 2; done
  ! pgrep -u "$OLD" >/dev/null 2>&1 || die "processes still run as $OLD: $(pgrep -u "$OLD" -l | tr '\n' ' ')"
  local builds; builds="$(running_builds)"
  if [ -n "$builds" ]; then
    # Each one's job is still in the database queue and is built again by the new daemon.
    log "stopping build containers: $builds"
    docker stop -t 10 $builds >/dev/null
  fi

  step "back up config and database to $backup"
  local data; data="$(current_data)"
  if [ -f "$OLD_ETC/config.yml" ]; then cp -p "$OLD_ETC/config.yml" "$backup/"; fi
  if [ -f "$data/$OLD.db" ]; then
    python3 - "$data/$OLD.db" "$backup/$OLD.db" <<'PY'
import sqlite3, sys
src = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
dst = sqlite3.connect(sys.argv[2])
src.backup(dst)
dst.close(); src.close()
PY
    log "database backed up ($(du -h "$backup/$OLD.db" | cut -f1))"
  fi

  step "old zrok shares: $OLD_SHARES"
  if user_exists "$OLD" && [ -d "$OLD_DATA/zrok2" ]; then
    if [ "$OLD_SHARES" = delete ]; then
      delete_tagged_shares "$OLD" "$OLD_DATA" "$OLD:"
    else
      log "left for $NEW to adopt:"
      tagged_shares "$OLD" "$OLD_DATA" "$OLD:" | sed 's/^/           /' || true
    fi
  else
    log "already moved; skipped"
  fi

  step "rename the service account"
  if user_exists "$OLD" && ! user_exists "$NEW"; then
    usermod -l "$NEW" "$OLD"
    groupmod -n "$NEW" "$OLD"
    usermod -d "$NEW_DATA" "$NEW"
    log "$OLD -> $NEW, uid $(id -u "$NEW"), groups $(id -Gn "$NEW")"
  else
    log "already $NEW"
  fi

  step "move directories"
  local pair src dst
  for pair in "$OLD_DATA:$NEW_DATA" "$OLD_ETC:$NEW_ETC"; do
    src="${pair%%:*}"; dst="${pair#*:}"
    if [ -d "$src" ]; then
      if [ -d "$dst" ]; then rmdir "$dst" 2>/dev/null || die "$dst exists and is not empty"; fi
      mv "$src" "$dst"
      log "$src -> $dst"
    else
      log "$dst already in place"
    fi
  done

  step "rename files inside"
  rename_gate_files "$NEW_DATA" "$OLD" "$NEW"
  if [ -f "$NEW_DATA/$OLD.db" ]; then
    rewrite_db "$NEW" "$NEW_DATA/$OLD.db" "$OLD_DATA/" "$NEW_DATA/"
    rename_db "$NEW_DATA" "$OLD" "$NEW"
  fi
  [ -f "$NEW_ETC/config.yml.pre-$NEW" ] || cp -p "$NEW_ETC/config.yml" "$NEW_ETC/config.yml.pre-$NEW"
  rewrite_config "$NEW_ETC/config.yml" "$OLD" "$NEW"

  step "install $NEW"
  if pkg_installed "$NEW"; then log "$(rpm -q "$NEW") already installed"; else rpm -Uvh "$rpm"; fi
  log "$(/usr/local/bin/$NEW version 2>&1 | head -1)"
  write_dropins

  if [ "$COPY_CACHES" = 1 ]; then
    step "build caches not yet copied"
    copy_caches
  fi

  step "doctor"
  runuser -u "$NEW" -- /usr/local/bin/$NEW doctor -config "$NEW_ETC/config.yml"

  step "start $NEW"
  start_units "$NEW"
  trap - ERR
  log "cutover finished $(date -u '+%Y-%m-%d %H:%M:%S UTC'). Restoring previews continues in the background; run verify."
}

cmd_verify() {
  step "units"
  for u in $(units_of "$NEW"); do log "$u: $(systemctl is-active "$u" || true)"; done
  curl -s -o /dev/null -w "           healthz %{http_code}\n" http://127.0.0.1:8471/healthz || true

  step "shares against the database"
  runuser -u "$NEW" -- /usr/local/bin/$NEW shares list -config "$NEW_ETC/config.yml" || true

  step "public URLs"
  local url
  probe() { printf '           %s  %s\n' "$(curl -s -o /dev/null -m 15 -w '%{http_code}' "$1" || echo ERR)" "$1"; }
  probe "https://${WEBHOOK_NAME:-$NEW}.shares.zrok.io/webhook/github"
  probe "https://${DASH_NAME:-$NEW-dash}.shares.zrok.io/"
  while read -r url; do [ -n "$url" ] && probe "$url"; done < <(python3 - "$NEW_DATA/$NEW.db" <<'PY'
import sqlite3, sys
c = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
for (u,) in c.execute("SELECT url FROM previews WHERE state='ready' AND url != '' ORDER BY url"):
    print(u)
PY
)
  log "a webhook path answers 4xx to a GET (it wants a signed POST); a preview should answer 200"

  step "journal, last 15 minutes"
  journalctl -u "$NEW" --since -15min --no-pager | grep -ciE 'level=(error|warn)' | sed 's/^/           warnings+errors: /' || true
}

cmd_rollback() {
  local data user
  trap 'printf "\nROLLBACK STOPPED during: %s. Fix and run rollback again.\n" "${CURRENT_STEP:-?}" >&2' ERR

  step "stop $NEW"
  stop_units "$NEW"
  systemctl disable $(units_of "$NEW") >/dev/null 2>&1 || true
  user="$(current_user)"
  for i in $(seq 1 15); do pgrep -u "$user" >/dev/null 2>&1 || break; sleep 2; done
  ! pgrep -u "$user" >/dev/null 2>&1 || die "processes still run as $user"

  step "delete the shares $NEW created"
  # docpreview cannot adopt them, and each holds the name docpreview republishes under. The
  # docpreview: shares, if they were adopted rather than deleted, are left for docpreview.
  data="$(current_data)"
  if command -v zrok2 >/dev/null && [ -d "$data/zrok2" ]; then
    delete_tagged_shares "$user" "$data" "$NEW:"
  else
    warn "cannot list shares (no zrok2 CLI or no $data/zrok2); $OLD will fail on any name an $NEW: share holds"
  fi

  step "remove $NEW"
  rm -rf "$UNITS_DIR/$NEW-webhook.service.d" "$UNITS_DIR/$NEW-dashboard.service.d"
  if pkg_installed "$NEW"; then rpm -e "$NEW"; fi

  step "rename files back"
  if [ -d "$NEW_DATA" ]; then
    rename_gate_files "$NEW_DATA" "$NEW" "$OLD"
    if [ -f "$NEW_DATA/$NEW.db" ]; then
      rewrite_db "$user" "$NEW_DATA/$NEW.db" "$NEW_DATA/" "$OLD_DATA/"
      rename_db "$NEW_DATA" "$NEW" "$OLD"
    fi
  fi
  if [ -f "$NEW_ETC/config.yml" ]; then rewrite_config "$NEW_ETC/config.yml" "$NEW" "$OLD"; fi

  step "move directories back"
  local pair src dst
  for pair in "$NEW_DATA:$OLD_DATA" "$NEW_ETC:$OLD_ETC"; do
    src="${pair%%:*}"; dst="${pair#*:}"
    if [ -d "$src" ] && [ ! -e "$dst" ]; then mv "$src" "$dst"; log "$src -> $dst"; else log "$dst already in place"; fi
  done

  step "rename the service account back"
  if user_exists "$NEW" && ! user_exists "$OLD"; then
    usermod -l "$OLD" "$NEW"
    groupmod -n "$OLD" "$NEW"
    usermod -d "$OLD_DATA" "$OLD"
    log "$NEW -> $OLD, uid $(id -u "$OLD")"
  fi

  step "$OLD package"
  if pkg_installed "$OLD"; then
    log "$(rpm -q "$OLD") still installed"
  else
    verify_rpm "$OLD_RPM"
    rpm -Uvh "$OLD_RPM"
  fi

  step "start $OLD"
  start_units "$OLD"
  trap - ERR
  log "rolled back. Pull requests built under $NEW keep that comment as well as the $OLD one."
}

cmd_finish() {
  systemctl is-active --quiet "$NEW" || die "$NEW is not running; finish is for after a good cutover"
  user_exists "$OLD" && die "the $OLD account still exists; cutover did not finish"
  step "remove the $OLD package"
  # Its preremove stops and disables the docpreview units, which are already stopped. Its
  # directories are gone, so nothing of the data is touched.
  if pkg_installed "$OLD"; then rpm -e "$OLD"; else log "not installed"; fi
  step "remove the $OLD build caches"
  local v
  for v in $(docker volume ls -q --filter "name=$OLD-cache-"); do
    case "$v" in "$OLD-cache-"*) docker volume rm "$v" >/dev/null && log "removed $v" ;; esac
  done
  log "kept: $STAGE (backups and the staged rpms) and $OLD_RPM, for a rollback after this"
}

case "${1:-}" in
  check) cmd_check ;;
  stage) cmd_stage ;;
  cutover) cmd_cutover ;;
  verify) cmd_verify ;;
  rollback) cmd_rollback ;;
  finish) cmd_finish ;;
  *) sed -n '3,15p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
