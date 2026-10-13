# ghostbranch sync: fast-forward each shadow branch in $BRANCHES from the
# public repo. Plain git and bash. It never force-pushes: every push is
# leased on the shadow commit just read, and anything but a fast-forward
# is reported, not pushed. Tests set PUBLIC_URL and SHADOW_URL.
set -uo pipefail
# Actions runs this as `bash -eo pipefail`, but every failure below is handled
# and reported on purpose, so errexit stays off. Globbing is off too, so a
# branch name can never expand in `for b in $BRANCHES`. Written for bash 3.2.
set +e
set -f
# An anonymous read of a private repo must fail, never wait at a prompt.
export GIT_TERMINAL_PROMPT=0

server="${GITHUB_SERVER_URL:-https://github.com}"
public_url="${PUBLIC_URL:-$server/$PUBLIC_REPO.git}"
shadow_url="${SHADOW_URL:-$server/${GITHUB_REPOSITORY:-}.git}"
summary="${GITHUB_STEP_SUMMARY:-/dev/null}"

# The token travels only in an http.extraheader, never in a URL. The derived
# base64 value is masked in the Actions log as well. The arrays stay empty when
# there is no token, and are expanded with ${arr[@]+"${arr[@]}"} so an empty
# one is safe under set -u on bash 3.2.
pub_auth=()
sh_auth=()
pub_token="${SYNC_TOKEN:-}"
sh_token="${SYNC_TOKEN:-${DEFAULT_TOKEN:-}}"
if [ -n "$pub_token" ]; then
  b64="$(printf 'x-access-token:%s' "$pub_token" | base64 | tr -d '\n')"
  echo "::add-mask::$b64"
  pub_auth=(-c "http.extraheader=AUTHORIZATION: basic $b64")
fi
if [ -n "$sh_token" ]; then
  b64="$(printf 'x-access-token:%s' "$sh_token" | base64 | tr -d '\n')"
  echo "::add-mask::$b64"
  sh_auth=(-c "http.extraheader=AUTHORIZATION: basic $b64")
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
git init -q --bare "$work/repo"
g() { git --git-dir="$work/repo" "$@"; }

failed=0
report() {
  echo "$1"
  echo "- $1" >> "$summary"
}
echo "### ghostbranch sync" >> "$summary"

if ! pub_refs="$(g ${pub_auth[@]+"${pub_auth[@]}"} ls-remote --heads "$public_url" 2>&1)"; then
  # git's first line says why; strip any user:password@ from URLs in it
  # (the auth is a header, so none is expected). No pipes: errexit+pipefail.
  why="${pub_refs%%$'\n'*}"
  why="$(sed -E 's#://[^/@[:space:]]+@#://#g' <<< "$why")"
  report "cannot read the public repo ${PUBLIC_REPO:-$public_url}: ${why}. If it is private: scheduled sync isn't supported for a private public repo (a token that could read it here could also write it); set sync.schedule: off, run ghostbranch upgrade, and run ghostbranch sync --repo ${PUBLIC_REPO:-<public>} locally. Nothing was pushed."
  exit 1
fi
if ! sh_refs="$(g ${sh_auth[@]+"${sh_auth[@]}"} ls-remote --heads "$shadow_url" 2>&1)"; then
  report "cannot read this shadow repo: $sh_refs"
  exit 1
fi
ref_of() { awk -v r="refs/heads/$2" '$2 == r { print $1 }' <<< "$1"; }

for b in $BRANCHES; do
  pub="$(ref_of "$pub_refs" "$b")"
  sh="$(ref_of "$sh_refs" "$b")"
  if [ -z "$pub" ]; then
    report "$b: the public repo has no branch $b; nothing was pushed."
    failed=1
    continue
  fi
  if [ "$sh" = "$pub" ]; then
    report "$b: up to date at ${pub:0:12}."
    continue
  fi
  if ! g ${pub_auth[@]+"${pub_auth[@]}"} fetch -q --no-tags "$public_url" "+refs/heads/$b:refs/public/$b" 2>/dev/null; then
    report "$b: could not fetch it from the public repo; nothing was pushed."
    failed=1
    continue
  fi
  if [ -n "$sh" ]; then
    if ! g ${sh_auth[@]+"${sh_auth[@]}"} fetch -q --no-tags "$shadow_url" "+refs/heads/$b:refs/shadow/$b" 2>/dev/null; then
      report "$b: could not fetch it from this shadow repo; nothing was pushed."
      failed=1
      continue
    fi
    if ! g merge-base --is-ancestor "$sh" "$pub" 2>/dev/null; then
      report "$b: diverged: shadow $b has commits that are not in public; nothing was pushed. A person must look; Ghostbranch never force-pushes."
      failed=1
      continue
    fi
  fi
  if out="$(g ${sh_auth[@]+"${sh_auth[@]}"} push --porcelain "--force-with-lease=refs/heads/$b:$sh" "$shadow_url" "$pub:refs/heads/$b" 2>&1)"; then
    if [ -z "$sh" ]; then
      report "$b: created at ${pub:0:12}."
    else
      report "$b: fast-forwarded ${sh:0:12}..${pub:0:12}."
    fi
    continue
  fi
  failed=1
  if grep -qi 'workflow' <<< "$out" && grep -qi 'permission' <<< "$out"; then
    report "$b: GitHub refused the push: the new public commits change workflow files, which this workflow's default token may not push. Run \`ghostbranch sync\` locally, or add a GHOSTBRANCH_SYNC_TOKEN secret with Workflows write on this repo. git said:"
  elif grep -q 'GH013' <<< "$out"; then
    report "$b: GitHub refused the push under a repository rule (GH013); run \`ghostbranch sync\` locally to see more. git said:"
  else
    report "$b: the push was refused; run \`ghostbranch sync\` locally to see more. git said:"
  fi
  printf '%s\n' "$out" | sed 's/^/    /'
done
exit "$failed"
