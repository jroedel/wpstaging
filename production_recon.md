# Production recon

What is known about the host and the site, so that questions about them can be
answered without asking anyone to run anything. Gathered by `deploy/recon.sh`.

**No credentials, hostnames of other accounts, or password hashes belong in this
file.** It is committed to a public repository. The real database name, user and
password live in the gitignored runtime config and nowhere else.

## The host

| | |
|---|---|
| Provider | Hetzner, managed through the **konsoleH** panel |
| Kind | Dedicated/managed server, not shared webhosting |
| Host | `dedi2934.your-server.de` — the same physical machine as the sibling `uebung` project, under a different konsoleH account |
| OS | Debian 12 (bookworm), kernel 6.1 |
| Resources | 24 CPUs, 62 GiB RAM, 269 GiB free on the site's filesystem |
| Account user | uid 1014, groups: its own and `users` |
| Home | under `/usr/home/`, not `/home/` |

Because it is the same hosting product as `uebung`, that project's `DEPLOYING.md`
and `production_recon.md` apply almost verbatim. Read them before designing
anything about deployment; nearly every question below was answered there first
and confirmed here.

## What the shell can and cannot do

| Capability | State |
|---|---|
| root / `sudo` | **no** |
| write `/etc/apache2` | **no** — `sites-enabled` is not even readable |
| `systemd` running | yes |
| `systemctl --user` | **unavailable** — no user D-Bus, no `loginctl`, so no lingering |
| `crontab` | **yes** (already used for `uebung`) |
| `screen`, `nohup`, `flock`, `setsid` | yes |
| `mysql`, `mysqldump`, `mariadb-dump` | yes |
| `rsync`, `tar`, `zstd`, `curl`, `git`, `openssl` | yes |
| `certbot` | no — TLS is issued through konsoleH's SSL Manager |
| Go toolchain | no — build locally, upload a static binary |
| `wp-cli` | not installed; it is a single phar and needs no privileges |
| max user processes | 2048 |

**Nothing here needs root.** konsoleH covers every privileged operation the
design requires: addon domains for the two subdomains, DNS, Let's Encrypt
certificates, and the document root, which is a panel setting rather than a file.

## Web serving

Apache 2.4.68 terminates TLS. The Go binary runs on loopback and is reached
through an `.htaccess` proxy — the shape `uebung` already uses on this host:

```
browser ──HTTPS──▶ Apache (konsoleH cert) ──.htaccess [P]──▶ 127.0.0.1:PORT ──▶ wpstaging
```

- **`ProxyPass` is illegal in `.htaccess`.** Use `RewriteRule … [P]`; `mod_proxy`
  is enabled for exactly this, and it is what Hetzner's own konsoleH
  documentation prescribes. `mod_rewrite` and `mod_headers` are available.
- `apache2ctl -M` cannot be run unprivileged, so the recon script's module list
  reads "ABSENT or not listable" for everything. That is the unprivileged case,
  not a missing module.
- **Non-standard ports are firewalled by default.** Irrelevant here: the binary
  must never be reachable except through Apache.
- `127.0.0.1:8402` is already in use on this machine. Pick something else.

## Keeping the process alive

`systemctl --user` does not work, so supervision is `nohup` plus cron, copied
from `uebung/deploy/supervise.sh`:

```
@reboot      <app dir>/supervise.sh start
*/5 * * * *  <app dir>/supervise.sh start     # idempotent: doubles as the watchdog
```

Three failure modes already paid for in the sibling project — do not rediscover
them:

- **`setsid` forks**, so `$!` is the wrapper, not the app. The launched script
  must record its own `$$` before `exec`, or the PID file points at a dead
  process, `stop` becomes a no-op, and the watchdog starts a second instance
  every five minutes.
- **A child inherits the `flock` fd.** Close it (`9>&-`) or the app holds the
  lock for its whole life and the next `start` blocks forever.
- **A child inheriting stdin holds the `ssh` channel open.** Redirect
  `</dev/null` or the deploy hangs immediately after starting the app.

## PHP

PHP 8.4.24. Values below are the **CLI's**; the FPM pool's have not yet been
read, and they are the ones that decide the release-swap strategy.

| Setting | CLI value | Consequence |
|---|---|---|
| `opcache.enable` | 1 | |
| `opcache.validate_timestamps` | 1 | |
| `opcache.revalidate_freq` | 2 | |
| `opcache.revalidate_path` | **0** | The stale-release problem is real: OPcache keys on the requested path, which never changes across a symlink flip |
| `opcache.restrict_api` | empty | `opcache_reset()` is callable from anywhere, so the reset-over-HTTP route is available |
| `user_ini.filename` | **empty** | `.user.ini` files appear to be **disabled**, which would rule out setting `revalidate_path` per-vhost |
| `memory_limit` | 96M | |

**Open question:** confirm `opcache.*` and `user_ini.filename` as FPM sees them,
with a temporary `phpinfo()` in the docroot. If `user_ini` really is disabled,
the swap must use `opcache_reset()` over a local HTTP request — which on a
shared pool also flushes the live site's opcode cache. A brief recompilation
cost, not a correctness problem, but it must be deliberate.

## The site

| | |
|---|---|
| Path | `/usr/home/<account>/public_html/es_new/` |
| Writable by the account | **yes**, mode 755 |
| Total size | 3.4 GB |
| Uploads | 1.8 GB across 6,524 files |
| Total files | 34,029 |
| Plugins / themes | 21 / 7 |
| WordPress | 7.0.4, single site |
| Table prefix | `wp_` |
| Filesystem | one ext4 volume for home and site, so `rename()` is atomic between the store and the docroot |

**Open question:** `es_new` looks like a language variant or an in-progress
rebuild rather than the main document root. The recon script found it by walking
the home directory, and `public_html/wp-config.php` did not exist. Confirm which
directory actually serves the production domain before pointing anything at it.

### Database privileges — decisive

```
GRANT USAGE ON *.* TO <user>@'%'
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, DROP, REFERENCES, INDEX, ALTER,
      CREATE TEMPORARY TABLES, LOCK TABLES, EXECUTE, CREATE VIEW, SHOW VIEW,
      CREATE ROUTINE, ALTER ROUTINE, EVENT, TRIGGER ON <one database>.*
```

`CREATE` and `DROP` are granted **within one database only**; there is no
`CREATE DATABASE`. This confirms the release model: each release gets its own
**table prefix** inside the existing database, not its own schema. `LOCK TABLES`
is available if a non-transactional table ever needs it.

**Note for the account holder:** MySQL is listening on `*:3306` rather than
loopback. Combined with a predictable username, that is an online brute-force
target if the port is reachable from outside the machine. Worth checking, and
unrelated to this project.

## Live-data census — this is not a static site

The single most important finding, and it invalidated an earlier design
assumption twice over. Production actively accumulates:

| What | Volume | Most recent |
|---|---|---|
| `flamingo_inbound` (Contact Form 7 message archive) | **9,690** posts | same day as the recon |
| `flamingo_contact` | 3,123 posts | same day |
| `wp_comments` | 635 — but **609 spam**, 23 approved, 3 pending | same day |
| `wp_commentmeta` | 2,353 | same day |
| `wp_actionscheduler_*` | background job queue, running | same day |
| `wp_options` | 832 rows, rewritten constantly by transients | same day |
| `shop_order` (WooCommerce) | 8 completed orders | 2025-03-15 |
| `wp_users` | 5 | 2025-12-22 |

Plugins in play: **WooCommerce**, Contact Form 7 + Flamingo, Action Scheduler,
Rank Math, The Events Calendar, Polylang (multilingual), YITH Wishlist, Google
Listings & Ads, Smart Slider, WPCode.

### Why this breaks whole-table preservation

Preserving `wp_comments` and `wp_commentmeta` is straightforward — they are
tables, and a table can be carried over from live wholesale.

**`flamingo_inbound` and `shop_order` are not tables. They are `post_type`
values, and they live in `wp_posts`** — the same table that holds the pages and
posts you edit on staging. There is no table-level split that keeps production's
new form submissions while taking staging's edited pages, because they are rows
in one table.

That is why promotion is tiered, and why the file-only tier is the default. See
the README.

## Consequences for the design, in one list

- Release isolation is by **table prefix**, confirmed by the grants.
- Deployment needs **no root**: konsoleH for domains, DNS, TLS and document
  roots; `.htaccess [P]` for the proxy; cron for supervision.
- The binary must **bind loopback**, be built `CGO_ENABLED=0`, shut down
  gracefully on SIGINT, and never infer cookie `Secure` from a forwarded header
  — all for the reasons `uebung/DEPLOYING.md` sets out.
- **Promotion to production must not move the database by default.** The site's
  database is live; its files are code.
- Snapshot size is comfortable: 3.4 GB against 269 GB free, before
  deduplication.
