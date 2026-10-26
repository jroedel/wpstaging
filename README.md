# wpstaging

Snapshot a WordPress site's whole state — files and database together — into an
immutable, content-addressed object. Deploy any state to staging or to
production with an atomic swap and a one-command rollback.

One static Go binary and a server-rendered web UI. It runs *beside* WordPress on
the same host, not inside it as a plugin, which is the whole point: the tools
that live inside WordPress inherit PHP's `max_execution_time` and `memory_limit`,
and that is why they cap out and time out on exactly the ten-year-old sites that
need them most.

## Why this exists

The obvious answer is "surely this is solved". It is solved in pieces, and the
pieces do not meet.

- **[WP Staging](https://wp-staging.com/)** clones a live site to a subdomain
  well, and it is GPL. Pushing the clone *back* to production is a Pro feature.
  So is exporting a backup. The free tier stops precisely at the interesting
  half.
- **[Duplicator Lite](https://duplicator.com/duplicator-free-vs-pro/)** packages
  files and database into one archive — the right idea — with a **500 MB** cap
  on the free tier. A decade of uploads clears that on media alone.
- **UpdraftPlus** backs up and restores for free; migrating a backup to a
  different URL needs the paid Migrator add-on. **All-in-One WP Migration**
  exports freely and throttles import.
- **[CaptainCore](https://docs.captaincore.io/)** is the closest thing in spirit:
  genuinely open source, self-hosted, a real web UI, restic-backed encrypted
  snapshots, file-level checkpoints. Its author labels it alpha and "not yet
  ready for public use", and it drives sites over SSH against Kinsta and WP
  Engine rather than a box you administer yourself.
- **[Bedrock/Trellis](https://roots.io/bedrock/docs/deployment/)** is the correct
  answer for *code* — Composer-managed WordPress, git deploys, real environments
  — and touches neither the database nor the uploads directory. Retrofitting it
  onto a legacy site is its own migration project.

Nothing open-source closes the loop: capture a full state, keep a library of
states cheaply, put any of them on staging, and put a tested one back on
production.

One piece of prior art is worth reusing rather than rewriting:
[`Automattic/go-search-replace`](https://github.com/Automattic/go-search-replace)
rewrites URLs in a WordPress SQL dump *and repairs the byte lengths inside PHP
serialized values*, which is the single sharpest edge in this entire problem.

## The thing this deliberately does not do

**It does not diff or merge databases.** Not "push these three pages to prod",
not "sync the changes back", not a change log of `UPDATE` statements to replay
against another environment.

This is a considered exclusion, not a missing feature:

- **VersionPress**, which versioned the WordPress database in git, ended
  development in June 2020.
- **Mergebot**, which merged database changes between environments, was shut
  down by Delicious Brains in August 2018 — a company whose entire business is
  WordPress database migration, killing their own product because they could not
  make it reliable.

Their [postmortem](https://deliciousbrains.com/syncing-wordpress-database-changes-merging/)
names the reasons, and none of them go away with more effort: every plugin
invents its own storage shape, auto-increment ids collide across environments,
references hide inside serialized blobs, and any plugin update can silently
change the format and break the merge logic for good.

So the unit of movement here is **the whole state**, with one escape hatch that
is carefully not a merge.

### Preserved tables: whole-table authority

Real sites accumulate data on production while you work on staging. This one
takes comments. Promoting a state that replaced `wp_comments` would delete every
comment posted since the snapshot, which is unacceptable and has nothing to do
with wanting a merge engine.

The answer is to decide authority **per table**, in configuration, once:

```toml
[env.production]
preserve = ["comments", "commentmeta"]
```

A preserved table is carried over from live production rather than taken from
the state. Every other table comes from the state. **No table is ever combined
from both sides**, no rows are compared, no ids are remapped, and the tool never
needs to know what a plugin meant by anything it wrote.

That is what makes this tractable where merging is not. The ids already agree,
because staging is a copy of production and nobody edits comments on staging, so
`comment_post_ID` points at the same posts on both sides. Two mechanical fixups
follow the swap:

- **Orphans are reported.** A comment on a post that staging deleted has nowhere
  to attach. WordPress tolerates it — the row simply stops displaying — but you
  should be told, so promotion prints the count and what it was attached to.
- **`comment_count` is recomputed.** It is a denormalised counter on `wp_posts`,
  so a preserved comment table and a state-supplied post table disagree about it
  until it is recalculated.

The honest limits, stated plainly: a table is preserved or it is not, so if you
need to edit comments *on staging* and keep production's new ones too, this
cannot help you and neither can anything else. And preserving a table whose rows
reference rows in a non-preserved table is only safe while the ids on both sides
share an ancestor — which holds for this workflow, where staging is always a
recent copy of production, and stops holding if a staging environment is kept
alive for months while production moves on.

### Where whole-table authority runs out

There is a case it cannot cover, and the site this was built for has it.

WordPress does not give every kind of record its own table. A contact-form
archive (`flamingo_inbound`), a WooCommerce order (`shop_order`), an event
(`tribe_events`) — these are not tables. They are `post_type` values, and they
are **rows in `wp_posts`**, alongside the pages and posts you went to staging to
edit.

So there is no table-level split that keeps production's new form submissions
while taking staging's rewritten pages. They are rows in one table, and choosing
that table means choosing one and discarding the other.

Row-level authority scoped by `post_type` would solve it, and it is a genuinely
harder problem than it looks: rows created on both sides claim overlapping
auto-increment ids, so carrying production's rows across means renumbering them
and rewriting every reference — `wp_postmeta.post_id`,
`wp_woocommerce_order_items.order_id`, and whatever a plugin invented. That is
the road with the wreckage on it. It is not ruled out forever, but it will not
be attempted casually, and it is not in v1.

What v1 does instead is refuse to put you in that position by default. See
below.

## Concepts

### State

A **state** is an immutable capture of everything a WordPress site *is* at one
moment:

- the filesystem tree — core, themes, plugins, mu-plugins, drop-ins, uploads
- a logical dump of the database
- a manifest: WordPress version, PHP version, table prefix, `siteurl` and
  `home`, active theme, active plugins, database charset and collation, the
  capture timestamp, the parent state, and the digest of all of it

States are named by their content digest and are never modified. You give them
human labels — `before-woo-upgrade`, `2016-theme-baseline` — and the labels move,
the states do not.

**A state is stored verbatim, with production's URLs intact.** No rewriting
happens at capture time. A state is a faithful record of what the site was;
rewriting is a property of *deploying* it somewhere, which is what lets one
state deploy to staging and to production without a second copy.

### Environment

An **environment** is a place a state can be deployed to. Declared in config,
not discovered:

```toml
[env.production]
docroot  = "/var/www/example.com"
url      = "https://example.com"
db_dsn   = "wp@unix(/run/mysqld/mysqld.sock)/wordpress"
# Tables carried over from live rather than taken from the state, because
# production keeps writing them while staging work is in progress.
preserve = ["comments", "commentmeta"]
opcache  = "revalidate-path"   # or "reset-over-http" where that is not settable

[env.staging]
docroot  = "/var/www/staging.example.com"
url      = "https://staging.example.com"
db_dsn   = "wpstg@unix(/run/mysqld/mysqld.sock)/wordpress"
opcache  = "revalidate-path"
safety   = ["noindex", "no-email", "basic-auth", "no-cron"]
```

Both live on the one dedicated server. Nothing here needs SSH, and the tool does
not open network connections to move data around — a staging deploy is a local
operation against the local filesystem and the local database server.

## Storage: content-addressed, deduplicated

The naive design writes a zip per state. At ten years of uploads that means
storing the same 20 GB of media on every snapshot, and you stop taking snapshots.

Instead, content is split into chunks with a rolling hash (FastCDC), each chunk
stored once under its SHA-256 digest and compressed with zstd. A state is a small
tree object pointing at chunks. Fifty snapshots of a 20 GB site cost roughly
20 GB plus the deltas, not a terabyte, so snapshotting before every change is
cheap enough that you actually do it.

Two consequences worth stating outright:

- **The database dump is generated deterministically** — tables in name order,
  rows in primary-key order, no dump timestamp, and `AUTO_INCREMENT` stripped
  from `CREATE TABLE` because it moves without the data moving. `mysqldump`
  writes a header containing the time of the dump and does not guarantee row
  order, so two dumps of an unchanged database differ, and every chunk after the
  first difference fails to deduplicate. Determinism is not tidiness here; it is
  what makes the storage model work, and it is why the dump is written by this
  tool against `go-sql-driver/mysql` rather than by shelling out.

  Measured, on a 12 MB dump held in 39 chunks: **one new comment cost one chunk
  and 2.3% of the stored size.** A dump and a restore of it produce byte-identical
  files, across emoji, umlauts, binary columns containing NUL, embedded newlines
  and NULLs.
- **`export` and `import` still exist.** A content-addressed store is excellent
  on the box and useless on a USB stick, so any state can be exported to a single
  portable archive — files, dump and manifest — and imported back into any store.
  That is the artifact you copy off-site, and it is
  [encrypted by default](#exports-are-secrets-and-are-encrypted).

Deleting a state unlinks it from the index; chunks that no state references are
reclaimed by `wpstaging gc`.

## Deploying: atomic swap with a per-release table prefix

A deploy never mutates a live docroot in place. There is no window during which
the site is half of one state and half of another.

1. Materialize the state into `releases/<state-short-id>/` beside the docroot.
2. Stream the state's dump through the URL rewrite — production URL to target
   URL, PHP serialized lengths repaired — into a **fresh set of tables** under
   the prefix `wpstg_<short-id>_`.
3. Write that release's own `wp-config.php`, setting `$table_prefix` to match.
4. Flip the docroot symlink to the new release and invalidate the opcode cache.

Because each release carries its own prefix in its own `wp-config.php`, the
symlink flip switches files and data together, in one operation. There is no
moment where new files are talking to old tables.

MySQL has no atomic "replace these tables", which is what motivates the whole
scheme — it converts an operation the database cannot do safely into a symlink
rename, which the filesystem can.

**Rollback is the same flip in reverse**, which is why the previous release and
its tables are kept rather than dropped. `wpstaging rollback <env>` is
sub-second and needs no archive to be unpacked. How many old releases to retain
is configurable; `gc` drops the rest, tables included.

**WordPress writes its prefix into data, not only into table names.** The
options table holds a `wp_user_roles` row, and user metadata holds
`wp_capabilities` and `wp_user_level` — all keyed by the prefix, and all
deciding whether anyone can log in. Rename the tables without rewriting those
rows and the site comes up with every account stripped of its role, including
yours. `foundation/wpdb` deliberately does not touch them: it moves bytes and
has no opinion about what WordPress means by any of them. The rewrite layer
above owns this, and a prefix change is not finished until it has run.

**A prefix rather than a separate database, deliberately.** A database per
release is the tidier model and needs `CREATE DATABASE`, which a great many
hosts reserve for their control panel and do not grant to the site's MySQL
account. A prefix needs only `CREATE TABLE` in a database you already have, so
the same code path works on a managed host and on a box you own. Where the
privilege does exist, `db_per_release = true` selects the tidier model.

### Invalidating the opcode cache without root

Step 4 has a sharp edge that is easy to miss. OPcache keys compiled scripts on
the path as requested, and with a docroot symlink that path is
`.../current/index.php` for every release that ever exists. Flip the symlink and
PHP happily keeps serving the previous release's compiled code.

Restarting PHP-FPM fixes it and needs root. Two routes that do not:

- **`opcache.revalidate_path=1`**, set for the environment. PHP then re-resolves
  the path rather than trusting its cached mapping. Costs a stat per include and
  fixes the problem outright; the catch is that it must be settable for that
  vhost, which depends on the host's `user_ini` configuration.
- **`opcache_reset()` over a local HTTP request**, from a single-purpose file
  placed in the new release and removed immediately after. Always available, but
  on a shared FPM pool it flushes *production's* opcode cache too — a brief
  recompilation cost on the live site, not a correctness problem, but not
  something to do silently either.

Which applies is a property of the host, not a matter of taste. `deploy/recon.sh`
reports the settings that decide it.

## Staging safety

A staging copy of a production site is a loaded weapon: it has the real customer
list, the real mail configuration and the real API keys, and it is about to be
exposed on the public internet. Safety measures are applied **during deploy, as
part of the transaction**, not left as a checklist:

- **`noindex`** — `blog_public` forced to `0`, an `X-Robots-Tag: noindex` header,
  and a `Disallow: /` robots.txt. A staging site that outranks production for its
  own content is a genuine and common outcome.
- **`no-email`** — a generated mu-plugin short-circuits `wp_mail()`, so a test
  order or a bulk re-save cannot mail real customers. Optionally routed to a
  catch-all address instead of dropped.
- **`basic-auth`** — HTTP basic auth in front of the whole environment by
  default. Public exposure was a requirement; public *readability* was not.
- **`disable-plugins`** — a configurable list switched off on arrival. Payment
  gateways, backup plugins on their own schedules, anything that fires webhooks
  at a third party.
- **`no-cron`** — `DISABLE_WP_CRON` set, so scheduled publishing and recurring
  jobs do not run twice.

These are per-environment flags. The production environment declares none of
them, and the tool refuses to apply them there.

## Promotion: the database is live, the files are code

The instinct is that promoting means moving the whole state back. For a site
with any life in it, that instinct is wrong, and the recon of the site this was
built for shows why: nearly ten thousand archived contact-form submissions, a
WooCommerce order history, a comment stream, and a background job queue — all of
it written by production, continuously, while you work on staging.

Meanwhile the changes you actually went to staging to make — a plugin updated, a
theme rewritten, a legacy page template deleted, custom CSS — are **files**.

So promotion is tiered, and the default moves no data at all:

```
wpstaging promote <state>                    files only; the database is untouched
wpstaging promote <state> --options a,b,c    files, plus named wp_options rows
wpstaging promote <state> --full             everything, with preserve lists and a freeze
```

**`promote` (files only) is the safe default and covers most of the work.**
Deploying a plugin's files to production is exactly what updating that plugin
normally does; WordPress notices the version change and runs the plugin's own
upgrade routine, the same as it would have. Nothing production wrote can be
lost, because nothing production wrote is touched. Rollback is a symlink flip.

**`--options`** carries named settings rows across for the case where the change
you tested *was* a setting. Named explicitly, one at a time, never wholesale:
`wp_options` also holds transients and cron state, and copying it entire would
drag a staging site's scheduled jobs onto production.

**`--full`** is the whole-state promotion described above, with preserved tables
and a content freeze. On a site like this one it will discard everything
production accumulated outside the preserved tables, so it demands explicit
confirmation and prints exactly what it is about to lose. It is the right tool
for a rebuild and the wrong tool for a Tuesday.

This is not a limitation being apologised for. It is how every mature deployment
system works: code moves forward, data does not.

## The content freeze, when you do promote in full

`--full` deploys a whole state to production using exactly the machinery above.
Two things happen around it that do not happen on a staging deploy:

- **Production is snapshotted first, automatically.** The pre-promotion state is
  captured and labelled before anything moves, so "undo the promotion" is always
  available as a deploy of a state, independent of the release-retention window.
- **The tool reports what production has accumulated** since the state being
  promoted was captured — row counts on comments, posts, users, and any table
  configured as live-writing — and requires confirmation naming that drift.

Everything not on the environment's `preserve` list comes from the state, so the
freeze covers posts, pages, media and settings: publish on production while a
staging cycle is open and promotion will overwrite it. Preserved tables are
exempt by construction — that is what they are for — which is why the drift
report separates the two. Drift in a preserved table is information; drift
anywhere else is about to be lost.

Keep staging cycles short. The mechanism above is sound, but every day a cycle
stays open is another day of production changes that promotion will discard, and
no amount of tooling makes that not true.

## Command surface

```
wpstaging snapshot <env> [--label NAME]     capture a state
wpstaging states [--env ENV]                list states, sizes, labels
wpstaging show <state>                      manifest and contents of a state
wpstaging diff <state-a> <state-b>          what changed: files, plugins, schema

wpstaging deploy <state> --to staging       materialize and swap
wpstaging promote <state>                   files only; production's data untouched
wpstaging promote <state> --options a,b     files, plus named wp_options rows
wpstaging promote <state> --full            everything, with preserves and a freeze
wpstaging rollback <env>                    flip back to the previous release

wpstaging export <state> --out FILE.age     portable archive, encrypted
wpstaging import FILE.age                   ingest one back
wpstaging gc                                reclaim unreferenced chunks/releases

wpstaging serve --addr 127.0.0.1:8099       the web UI
```

The web UI is the same operations over a timeline of states: what exists, what is
deployed where, what changed between two states, and the buttons to deploy,
promote and roll back — with deploy progress streamed over SSE. Server-rendered
`html/template` and Go's stdlib router; no JavaScript framework, no build step.
It binds to loopback and expects a TLS proxy in front, on the same reasoning as
any other admin surface.

## Running it on the server

Three names on one host: the live site, a staging subdomain, and a subdomain
serving this tool's UI. Apache terminates TLS for all three.

```
browser ──HTTPS──▶ Apache (panel-issued cert) ──.htaccess [P]──▶ 127.0.0.1:PORT ──▶ wpstaging
```

**The whole of this runs without root**, which is not a compromise but the
target: the reference deployment is a Hetzner konsoleH account where nobody has
root and the panel owns domains, DNS, certificates and document roots. That
constraint is worth designing to even where root is available, because it is the
common case for the sites that need this tool most.

- **The proxy lives in `.htaccess`.** `ProxyPass` is illegal there; the working
  form is `RewriteRule … [P]`, which `mod_proxy` supports for exactly this.
- **Supervision is cron.** Where `systemctl --user` is unavailable — no user
  D-Bus, no lingering — an `@reboot` entry plus an idempotent five-minute
  watchdog keeps the process up. Detached processes do survive an SSH
  disconnect.
- **The binary binds `127.0.0.1` and never opens a public port.** It never
  speaks TLS and never sits inside a document root.

The management UI is the most dangerous surface on the machine — more so than
`wp-admin`, which can edit a page, where this can replace the database — so it
gets two independent locks: HTTP authentication at the proxy, and the binary's
own session on top. One misplaced directive then costs a layer rather than the
site.

Two things that bite:

- **Exempt `/.well-known/acme-challenge/` from authentication.** ACME validation
  is an unauthenticated GET. Put HTTP auth across `/` and certificate renewal
  fails silently — sixty days later, quite far from the change that caused it.
- **A shared PHP-FPM pool means staging can take production down.** Separate
  pools need root and are usually unavailable. Without them, staging competes
  for the same workers as the live site, so keep the safety measures on
  (`no-cron` especially — a doubled scheduler is the usual way a staging copy
  starts consuming real resources) and remember that a heavy import on staging
  is felt by real visitors.

`deploy/recon.sh` surveys a host and reports which of the assumptions above hold
on it — opcode cache settings, database privileges, whether a long-lived process
can be kept alive, and which tables production actually writes. It reads and
prints; it changes nothing, needs no root, and redacts credentials. Run it
before writing any config. `production_recon.md` records what it found for the
reference deployment.

## Layout

Ardan Labs layering, as in the sibling project — primitives at the edges, strong
types only in Business, every crossing through a named converter.

```
app/
  stateapp/       HTTP handlers: snapshot, list, show, diff
  deployapp/      HTTP handlers: deploy, promote, rollback
  webui/          templates and static assets
business/
  domain/
    state/        statebus + stores (index)
    site/         sitebus — environments, WordPress introspection
    deploy/       deploybus + stores (releases, history)
  types/
    stateid/      content digest as a validated strong type
    envname/      environment identifier
    tableprefix/  validated wp_ prefix
foundation/
  cas/            chunking, compression, index, gc
  wpdb/           deterministic logical dump and restore
  wpfs/           tree walk, ignore rules, ownership, atomic materialize
  serialize/      PHP-serialization-safe search and replace
  errs/  web/  sqldb/
cmd/
  wpstaging/      the binary
```

The state index is SQLite via `modernc.org/sqlite` — pure Go, so the binary
still builds with no C toolchain.

## Scope

**In, for v1:**

- Snapshot, list, show, diff, gc
- Export and import, encrypted with age by default
- Content-addressed deduplicated store with deterministic database dumps
- Deploy to staging on the same host, atomic swap, per-release table prefix
- All five staging safety measures
- Tiered promotion: files-only by default, named options, or full state
- Automatic pre-promotion snapshot and drift reporting
- Preserved tables, so production's comments survive a full promotion
- Rollback
- Runs unprivileged: no root, no systemctl, no vhost edits at deploy time
- Web UI over all of it
- MySQL and MariaDB; single-site WordPress

**Out, for v1:**

- Database diffing or merging, in any form — see above. Preserved tables are
  whole-table authority, which is a different thing and the only thing offered.
- Row-level authority scoped by `post_type`, which is what carrying production's
  form submissions and orders across a full promotion would need. Understood,
  wanted, and deliberately not attempted in v1.
- Multisite
- Remote hosts, SSH, cloud storage back-ends
- PostgreSQL, SQLite-backed WordPress
- Scheduled/automatic snapshots — cron plus the CLI covers it until it doesn't

## Status

Scoping. No code yet beyond the module skeleton. This README is the plan, and it
is meant to be argued with before anything is built against it.

## Exports are secrets, and are encrypted

A state contains `wp-config.php` — database credentials, authentication salts,
and whatever API keys the site keeps there — plus anything equivalent in the
options table. The store on disk is protected by filesystem permissions. **An
export is not**: it is the artifact you copy to another disk, another machine or
a cloud bucket, which is exactly the moment those permissions stop applying.

So `wpstaging export` encrypts by default, using [age](https://age-encryption.org)
— passphrase or recipient key — and `--plaintext` is an explicit opt-out that
says so on the way past. `import` detects which it is given.

The state itself is stored **byte-faithful**, secrets and all. Scrubbing
`wp-config.php` and re-injecting per environment was considered and rejected: it
would mean a state is no longer a true record of the site, and a restore is only
worth trusting if what comes back is what was there. Encryption protects the
artifact without weakening what the artifact *is*.

## License

GPL-3.0. Nothing here links WordPress code, so copyleft is not required — it is
a choice to sit alongside the ecosystem this serves, where WordPress itself and
every tool named above are GPL.

## Open questions

1. **Chunking uniformity.** FastCDC over everything is one code path and dedupes
   the database dump beautifully; whole-file addressing for the uploads tree
   would be simpler and nearly as effective *there*, at the cost of running two
   mechanisms. Current plan is FastCDC everywhere, revisited if the small-file
   overhead shows up in measurement rather than in speculation.
