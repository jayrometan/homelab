# GitLab CE — Deployment & Operations Guide

Deployed on **192.168.1.26 (jay2)** — Rocky Linux 9.3, 4 vCPU, 3.6GB RAM  
Installation method: **GitLab Omnibus RPM**  
GitLab version: **19.1.1-ce**

---

## What Is GitLab Omnibus

GitLab Omnibus is a self-contained installation package that bundles everything GitLab needs into a single RPM:

- **GitLab Rails** — the main web application (Ruby on Rails)
- **Puma** — Ruby web server (replaced Unicorn in GL 13+)
- **Sidekiq** — background job processor (CI pipelines, emails, etc.)
- **PostgreSQL** — GitLab's database (bundled, separate from your own Postgres)
- **Redis** — caching and job queue backend
- **Nginx** — reverse proxy in front of Puma
- **Gitaly** — Git RPC service (handles all git operations)
- **GitLab Workhorse** — smart reverse proxy for large uploads/downloads
- **Prometheus** — metrics (can be disabled to save RAM)
- **Mattermost** — optional chat (we don't enable this)

All managed by a single tool: `gitlab-ctl`

---

## Installation Steps (Rocky Linux 9)

### 1. Prerequisites

```bash
dnf install -y curl policycoreutils openssh-server perl postfix
systemctl enable --now sshd postfix
systemctl disable --now firewalld
```

> **Why disable firewalld?** Same reason as on the k3s node — it blocks internal service communication. In production, configure firewalld zones for GitLab ports instead of disabling it.

### 2. Add the GitLab CE repository

```bash
curl -sS https://packages.gitlab.com/install/repositories/gitlab/gitlab-ce/script.rpm.sh | bash
```

### 3. Install GitLab CE

```bash
EXTERNAL_URL="http://192.168.1.26" dnf install -y gitlab-ce
```

`EXTERNAL_URL` is baked into the config at install time — GitLab uses it for generated URLs (clone URLs, webhook callbacks, etc.).

This takes 3–5 minutes. The RPM installs and runs `gitlab-ctl reconfigure` automatically on first install.

### 4. Tune for low memory (critical on 3.6GB RAM)

GitLab's defaults assume 8GB+ RAM. Edit `/etc/gitlab/gitlab.rb`:

```ruby
# /etc/gitlab/gitlab.rb

# Reduce Puma web workers (default: CPU count)
puma['worker_processes'] = 2

# Reduce Sidekiq background job concurrency (default: 25)
sidekiq['max_concurrency'] = 10

# PostgreSQL shared buffers
postgresql['shared_buffers'] = "256MB"

# Disable built-in Prometheus stack (saves ~200MB)
prometheus_monitoring['enable'] = false

# Reduce memory allocator fragmentation
gitlab_rails['env'] = {
  'MALLOC_CONF' => 'dirty_decay_ms:1000,muzzy_decay_ms:1000'
}
```

Apply changes:

```bash
gitlab-ctl reconfigure
```

### 5. Get the initial root password

```bash
cat /etc/gitlab/initial_root_password
```

This file is auto-deleted after 24 hours. **Change the root password immediately after first login.**

### 6. Access GitLab

Open `http://192.168.1.26` in your browser.  
Login: `root` / `<password from above>`

---

## Key Files and Directories

| Path | Purpose |
|------|---------|
| `/etc/gitlab/gitlab.rb` | Main config file — all tuning goes here |
| `/etc/gitlab/initial_root_password` | Auto-generated root password (deleted after 24h) |
| `/var/opt/gitlab/` | All GitLab runtime data (repos, DB, uploads) |
| `/var/opt/gitlab/postgresql/data/` | Bundled PostgreSQL data |
| `/var/opt/gitlab/git-data/repositories/` | Bare git repos |
| `/var/log/gitlab/` | All service logs |
| `/opt/gitlab/` | GitLab binaries and embedded runtime |

---

## gitlab-ctl — The Management CLI

Everything is managed via `gitlab-ctl`:

```bash
# Status of all services
gitlab-ctl status

# Start / stop / restart all services
gitlab-ctl start
gitlab-ctl stop
gitlab-ctl restart

# Restart a single service
gitlab-ctl restart puma
gitlab-ctl restart sidekiq
gitlab-ctl restart gitaly

# Tail all logs
gitlab-ctl tail

# Tail a specific service log
gitlab-ctl tail nginx
gitlab-ctl tail puma
gitlab-ctl tail postgresql

# Apply config changes from gitlab.rb (safe to run repeatedly)
gitlab-ctl reconfigure

# Run health checks
gitlab-rake gitlab:check
gitlab-rake gitlab:env:info
```

---

## Architecture — How GitLab Omnibus Hangs Together

```
Browser / Git client
        │
        ▼
    Nginx :80/:443
        │
        ├─────────────────────────────────┐
        ▼                                 ▼
  GitLab Workhorse                     (static assets)
  (large file uploads,
   git HTTP clone/push)
        │
        ▼
    Puma (Rails app)
        │
        ├──► Redis (session cache, Sidekiq queue)
        │         │
        │         ▼
        │      Sidekiq (background jobs)
        │      - CI pipeline triggers
        │      - Email notifications
        │      - Webhook delivery
        │      - Repository imports
        │
        ├──► PostgreSQL (metadata, users, issues, MRs, CI config)
        │
        └──► Gitaly (all git operations)
                   │
                   ▼
            /var/opt/gitlab/git-data/
            (bare git repositories on disk)
```

**Key insight:** GitLab splits concerns between PostgreSQL (structured metadata) and Gitaly (git object storage). Gitaly replaced direct git calls in the Rails app — it's a gRPC service that abstracts all git I/O. This matters for scalability (Gitaly can run on a dedicated node in large installs).

---

## systemd Integration

GitLab Omnibus registers itself as a systemd service:

```bash
systemctl status gitlab-runsvdir
systemctl enable gitlab-runsvdir    # auto-start on boot
```

Under the hood, Omnibus uses **runit** (a process supervisor) to manage all sub-services. `gitlab-ctl` is a wrapper around runit. Systemd just starts the runit supervisor (`gitlab-runsvdir`), and runit manages the rest.

```
systemd
  └─► gitlab-runsvdir  (runit supervisor)
        ├─► puma
        ├─► sidekiq
        ├─► postgresql
        ├─► redis
        ├─► nginx
        ├─► gitaly
        └─► gitlab-workhorse
```

---

## First-Time Setup Checklist

After getting GitLab running:

1. **Change root password** — Admin Area → Edit Profile
2. **Set up SSH keys** — Profile → SSH Keys (add your `~/.ssh/id_ed25519.pub`)
3. **Create a group** — organises repos by team (e.g. `homelab`, `platform`)
4. **Create a project** — this is a git repo
5. **Configure email** (optional for lab) — `/etc/gitlab/gitlab.rb`:
   ```ruby
   gitlab_rails['smtp_enable'] = false   # disable outbound email for lab
   ```
   Then `gitlab-ctl reconfigure`
6. **Enable GitLab CI** — it's on by default; runners need to be registered separately

---

## Registering a GitLab Runner

GitLab CI needs a **runner** to execute jobs. For a lab, register a runner on the same node:

```bash
# Install runner
curl -L https://packages.gitlab.com/install/repositories/runner/gitlab-runner/script.rpm.sh | bash
dnf install -y gitlab-runner

# Register (get token from GitLab UI: Admin → CI/CD → Runners → New runner)
gitlab-runner register \
  --url http://192.168.1.26 \
  --token <registration-token> \
  --executor shell \
  --description "jay2-shell-runner"

# Start the runner
systemctl enable --now gitlab-runner
```

For a k8s-based runner (what Tower would use): install the `gitlab-runner` Helm chart on the k3s cluster and point it at this GitLab instance.

---

## Integration with Your k3s Cluster (192.168.1.25)

For the full Tower-like setup:

1. **Store cluster manifests in GitLab** — push your k3s manifests/Helm values to a repo here
2. **GitLab CI deploys to k3s** — runner uses `kubectl` with a kubeconfig secret
3. **FluxCD watches GitLab** — configure FluxCD's `GitRepository` source to point at this GitLab instance instead of GitHub:

```yaml
# On the k3s cluster (192.168.1.25)
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: homelab
  namespace: flux-system
spec:
  interval: 1m
  url: http://192.168.1.26/root/homelab.git
  ref:
    branch: main
```

This mirrors how Tower likely has FluxCD pulling from their internal GitLab instance.

---

## Production Gotchas

**1. `gitlab-ctl reconfigure` is idempotent but slow**  
It re-renders all config templates and restarts changed services. Expect 1–2 minutes. Always run it after editing `gitlab.rb`.

**2. Backup before upgrades**  
```bash
gitlab-backup create
# Backup lands in /var/opt/gitlab/backups/
```
GitLab upgrades must follow the upgrade path — you can't skip major versions (e.g. 16 → 17 is fine, 15 → 17 is not).

**3. Gitaly is the memory hog**  
On constrained nodes, Gitaly can use 500MB+. If memory pressure is severe:
```ruby
gitaly['configuration'] = {
  concurrency: [
    { rpc: "/gitaly.SmartHTTPService/PostReceivePack", max_per_repo: 3 },
  ]
}
```

**4. PostgreSQL is separate from your StackGres clusters**  
GitLab's bundled Postgres is managed entirely by Omnibus. Don't connect to it directly or try to manage it with StackGres — they're completely separate.

**5. SELinux on Rocky 9**  
GitLab's RPM ships SELinux policies. If you see permission denied errors in logs after install, check:
```bash
ausearch -m avc -ts recent | audit2why
```

**6. Initial `reconfigure` vs subsequent ones**  
The first `reconfigure` (triggered by `dnf install`) sets everything up from scratch including generating secrets, certificates, and database initialization. Subsequent `reconfigure` runs are incremental and safe.

---

## Useful One-Liners

```bash
# Check all service health at once
gitlab-ctl status

# Watch memory usage of GitLab processes
watch -n2 'ps aux --sort=-%mem | head -20'

# Check GitLab version
gitlab-rake gitlab:env:info | grep "GitLab information" -A5

# Reset root password from CLI
gitlab-rake "gitlab:password:reset[root]"

# Check PostgreSQL is healthy
gitlab-psql -c "SELECT version();"

# Tail CI job logs in real time
gitlab-ctl tail sidekiq

# Disk usage breakdown
du -sh /var/opt/gitlab/*/
```

---

## GitLab Runner Setup

### What is a GitLab Runner

A GitLab Runner is an agent that picks up CI/CD jobs from GitLab and executes them. The runner polls GitLab for pending jobs, runs them in its configured executor (shell, Docker, Kubernetes, etc.), and streams logs back.

In GitLab 16+, the registration flow changed. You now create a runner object **server-side first** (via UI or API), which gives you a runner authentication token (`glrt-...`). You then register the runner binary using that token. Tags, run-untagged settings, and access level are all configured on the server side — you can't pass them at registration time.

### Architecture

```
GitLab Server (192.168.1.26)
  └─► Job queue (Redis)
        │
        ▼ (runner polls every few seconds)
GitLab Runner daemon (jay2, runs as gitlab-runner user)
  └─► Shell executor
        └─► Runs .gitlab-ci.yml jobs as shell processes
              ├─ git clone the repo
              ├─ execute each script step
              └─ upload artifacts back to GitLab
```

### Installation (what we did)

#### Step 1 — Create a Personal Access Token via Rails console

Needed to call the GitLab API without clicking through the UI:

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

This prints a `glpat-...` token.

> **Why Rails console?** GitLab's web UI requires an active session. For scripted/automated setup, the Rails runner lets you execute Ruby directly against the GitLab application.

#### Step 2 — Create the runner object on the server via API

```bash
curl --request POST 'http://192.168.1.26/api/v4/user/runners' \
  --header 'PRIVATE-TOKEN: <your-glpat-token>' \
  --form 'runner_type=instance_type' \
  --form 'description=jay2-shell-runner' \
  --form 'tag_list=shell,homelab,rocky9'
```

This returns a `glrt-...` runner authentication token. Instance runners are shared across all projects on the GitLab instance.

#### Step 3 — Install the gitlab-runner package

```bash
curl -sL https://packages.gitlab.com/install/repositories/runner/gitlab-runner/script.rpm.sh | bash
dnf install -y gitlab-runner
```

#### Step 4 — Register the runner

```bash
gitlab-runner register \
  --non-interactive \
  --url 'http://192.168.1.26' \
  --token 'glrt-<your-runner-token>' \
  --executor 'shell' \
  --description 'jay2-shell-runner'
```

> **Note:** With `glrt-...` tokens, do NOT pass `--tag-list`, `--locked`, or `--run-untagged` here. Those are set server-side (either via API form fields or the GitLab UI). Passing them causes a fatal error.

Config is saved to `/etc/gitlab-runner/config.toml`.

#### Step 5 — Enable and start

```bash
systemctl enable --now gitlab-runner
gitlab-runner status
```

#### Step 6 — Verify it's online

```bash
curl -s --header 'PRIVATE-TOKEN: <glpat-token>' \
  'http://192.168.1.26/api/v4/runners/all' | python3 -m json.tool
# Look for "status": "online"
```

Or in the GitLab UI: **Admin Area → CI/CD → Runners**

---

### Using the Runner — Example `.gitlab-ci.yml`

Create a project in GitLab, push this file, and the runner will pick it up:

```yaml
# .gitlab-ci.yml
stages:
  - test
  - deploy

test-job:
  stage: test
  tags:
    - shell          # matches our runner's tags
  script:
    - echo "Running on $(hostname)"
    - echo "Branch: $CI_COMMIT_BRANCH"
    - echo "Commit: $CI_COMMIT_SHA"

deploy-to-k3s:
  stage: deploy
  tags:
    - shell
  only:
    - main
  script:
    - kubectl apply -f k8s/   # works if kubeconfig is set on the runner host
```

### Runner Executor Types (and when to use them)

| Executor | How it runs jobs | Use case |
|----------|-----------------|----------|
| `shell` | Directly on the runner host as the `gitlab-runner` user | Simple scripts, kubectl, ansible — what we use |
| `docker` | Each job in a fresh Docker container | Isolated builds, language-specific toolchains |
| `kubernetes` | Each job as a Kubernetes Pod | What Tower likely uses — clean isolation, scales |
| `ssh` | SSH into a remote machine and run there | Legacy systems |

**Tower's likely setup:** Kubernetes executor running on the k3s (or Kubespray) cluster. Each CI job gets a dedicated pod that's destroyed after the job. Clean, isolated, and scales with the cluster.

### Runner Files

| Path | Purpose |
|------|---------|
| `/etc/gitlab-runner/config.toml` | Runner configuration (registered runners, executor settings) |
| `/var/log/gitlab-runner/` | Runner logs (also via `journalctl -u gitlab-runner`) |
| `/home/gitlab-runner/` | Runner's home dir, job workspaces land here |

### Key Runner Commands

```bash
# Check runner service
systemctl status gitlab-runner
journalctl -u gitlab-runner -f

# List registered runners
gitlab-runner list

# Verify registered runners can reach GitLab
gitlab-runner verify

# Run a job locally for debugging (without GitLab)
gitlab-runner exec shell <job-name>

# Unregister a runner
gitlab-runner unregister --name jay2-shell-runner
```
