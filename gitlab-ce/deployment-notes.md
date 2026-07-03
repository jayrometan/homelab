# GitLab CE — Actual Deployment Session Notes

This captures what was actually run during deployment, including issues hit and how they were resolved. Use this as a real-world reference alongside the main README.

**Date:** 2026-07-04  
**Host:** 192.168.1.26 (jay2) — Rocky Linux 9.3  
**Starting resources:** 2 vCPU, 3.6GB RAM  
**Final resources:** 4 vCPU, 3.6GB RAM (added CPU mid-session)  
**GitLab CE version installed:** 19.1.1

---

## Phase 1 — Prerequisites

```bash
dnf install -y curl policycoreutils openssh-server perl postfix
systemctl enable --now sshd postfix
systemctl disable --now firewalld
```

**Why firewalld was disabled upfront:** We hit this exact issue on the k3s node (jay1) — Rocky 9's firewalld blocks internal service-to-service traffic that GitLab's components rely on. Disabling it before install avoids services starting with broken connectivity.

---

## Phase 2 — Add Repo and Install

```bash
curl -sS https://packages.gitlab.com/install/repositories/gitlab/gitlab-ce/script.rpm.sh | bash

EXTERNAL_URL="http://192.168.1.26" dnf install -y gitlab-ce
```

**Download size:** ~942MB RPM (`gitlab-ce-19.1.1-ce.0.el9.x86_64.rpm`)  
**Install time:** ~8 minutes (download + install + initial reconfigure)

The `EXTERNAL_URL` env var is baked into `/etc/gitlab/gitlab.rb` at install time. GitLab uses it to generate clone URLs, webhook URLs, and OAuth callbacks. If you change it later, edit `gitlab.rb` and run `gitlab-ctl reconfigure`.

The RPM install automatically runs `gitlab-ctl reconfigure` at the end — this initialises the database, generates internal secrets and certificates, and starts all services. You don't need to run it manually after fresh install.

---

## Phase 3 — Initial Service State

After install completed, service status:

```
run: gitaly
run: gitlab-exporter
run: gitlab-kas
run: gitlab-workhorse
run: logrotate
run: nginx
run: node-exporter
run: postgresql
run: prometheus          ← consuming ~200MB
run: puma               ← 2 workers by default
run: redis
run: redis-exporter
run: sidekiq            ← 25 concurrent threads by default
```

**Memory at this point:** 3.2GB / 3.6GB used — critically tight.

---

## Phase 4 — Memory Tuning (single-user homelab)

The default GitLab config assumes 8GB+ RAM. For a single-user lab on 3.6GB, we disabled non-essential services and reduced worker counts.

Appended to `/etc/gitlab/gitlab.rb`:

```ruby
# ---- Single-user homelab minimal config ----
# Puma single mode: no worker processes, just the master
# Fine for one user, saves ~400MB vs default
puma['worker_processes'] = 0

# Minimal background job concurrency
sidekiq['max_concurrency'] = 5

# Reduce PostgreSQL footprint
postgresql['shared_buffers'] = '128MB'
postgresql['max_connections'] = 50

# Disable the entire metrics/monitoring stack
prometheus_monitoring['enable'] = false
node_exporter['enable'] = false
redis_exporter['enable'] = false
gitlab_exporter['enable'] = false

# Disable Kubernetes agent server (not needed for basic lab)
gitlab_kas['enable'] = false

# Allocator tuning to reduce fragmentation overhead
gitlab_rails['env'] = {
  'MALLOC_CONF' => 'dirty_decay_ms:1000,muzzy_decay_ms:1000'
}
# ---- end single-user config ----
```

Applied with:
```bash
gitlab-ctl reconfigure
```

**Memory after tuning:** ~2.5GB used, 1.1GB free — comfortable.

**Services still running after tuning:**
```
run: gitaly
run: gitlab-workhorse
run: logrotate
run: nginx
run: postgresql
run: puma
run: redis
run: sidekiq
```

---

## Issue Encountered — SELinux

**Problem:** Mid-session, GitLab services were misbehaving with permission errors.  
**Cause:** SELinux was set to `Enforcing` on the Rocky 9 VM.  
**Resolution:** Disabled SELinux (requires reboot to take effect):

```bash
# Edit /etc/selinux/config:
SELINUX=disabled

# Then reboot the VM
reboot
```

After reboot, confirmed with:
```bash
getenforce
# Disabled
```

**Production note:** In a production deployment, you wouldn't disable SELinux — you'd use GitLab's bundled SELinux policy (included in the RPM) and troubleshoot denials with `ausearch -m avc -ts recent | audit2why`. Disabling is fine for a homelab.

After reboot, GitLab auto-started via systemd (`gitlab-runsvdir` service).

---

## Phase 5 — GitLab Runner Setup

### Why GitLab 16+ Changed Runner Registration

In older GitLab versions, you'd get a "registration token" from the UI and pass it to `gitlab-runner register`. This token was static and had no expiry — a security risk.

From GitLab 16+, the model changed:
- You **create the runner object server-side first** (via UI or API)
- This returns a **runner authentication token** (`glrt-...`)
- You register the runner binary with that token
- Tags, access level, run-untagged settings are set **server-side**, not at register time

### Step 1 — Create an API token via Rails console

```bash
gitlab-rails runner "
token = User.find_by_username('root').personal_access_tokens.create(
  name: 'automation',
  scopes: [:api, :read_user],
  expires_at: 365.days.from_now
)
puts token.token
" 2>/dev/null
```

Returns a `glpat-...` token.

### Step 2 — Create runner object via API

```bash
curl --request POST 'http://192.168.1.26/api/v4/user/runners' \
  --header 'PRIVATE-TOKEN: <glpat-token>' \
  --form 'runner_type=instance_type' \
  --form 'description=jay2-shell-runner' \
  --form 'tag_list=shell,homelab,rocky9'
```

Returns:
```json
{"id":1,"token":"glrt-...","token_expires_at":null}
```

### Step 3 — Install gitlab-runner package

```bash
curl -sL https://packages.gitlab.com/install/repositories/runner/gitlab-runner/script.rpm.sh | bash
dnf install -y gitlab-runner
```

### Step 4 — Register the runner binary

```bash
gitlab-runner register \
  --non-interactive \
  --url 'http://192.168.1.26' \
  --token 'glrt-<runner-auth-token>' \
  --executor 'shell' \
  --description 'jay2-shell-runner'
```

**Issue hit:** First attempt included `--tag-list` flag and got:

```
FATAL: Runner configuration other than name and executor configuration is reserved
(specifically --locked, --access-level, --run-untagged, --maximum-timeout,
--paused, --tag-list, and --maintenance-note) and cannot be specified when
registering with a runner authentication token.
```

**Fix:** Remove `--tag-list` from the register command. Tags were already set in Step 2 via the API `--form 'tag_list=...'` — no need to repeat them.

### Step 5 — Enable and verify

```bash
systemctl enable --now gitlab-runner

# Verify it's online
curl -s --header 'PRIVATE-TOKEN: <glpat-token>' \
  'http://192.168.1.26/api/v4/runners/all' | python3 -m json.tool
```

Expected output includes `"status": "online"`.

---

## Final State

```
GitLab CE 19.1.1   running at http://192.168.1.26
GitLab Runner      online, shell executor, instance runner (shared across all projects)
```

**Access:**
- URL: `http://192.168.1.26`
- Default user: `root`
- Initial password: stored in `/etc/gitlab/initial_root_password` (auto-deleted 24h after install)
- Reset password anytime: `gitlab-rake "gitlab:password:reset[root]"`

**Runner config file:** `/etc/gitlab-runner/config.toml`

---

## Key Lessons From This Deployment

1. **Disable firewalld before installing GitLab on Rocky 9** — or add explicit rules for the ports GitLab uses internally. Waiting until after install means services start but can't talk to each other.

2. **SELinux requires a reboot** to switch from Enforcing to Disabled. A `setenforce 0` temporarily disables enforcement but doesn't persist across reboots.

3. **GitLab's default config is sized for 8GB+ RAM.** On a small VM, disable prometheus_monitoring, node/redis/gitlab exporters, gitlab-kas, and set `puma['worker_processes'] = 0`. You'll go from 3.2GB → ~2.5GB used.

4. **GitLab Runner `glrt-...` tokens don't accept config flags at register time.** All runner config (tags, locked, run-untagged) is managed server-side via the API or UI. This is a common stumbling block after the GitLab 16 registration model change.

5. **The Rails console is your escape hatch.** For any admin operation you'd normally do via the UI (create tokens, reset passwords, manipulate objects), `gitlab-rails runner "..."` lets you do it programmatically.
