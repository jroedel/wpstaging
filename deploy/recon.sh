#!/usr/bin/env bash
#
# recon.sh -- read-only survey of a WordPress host, run by a human.
#
# wpstaging makes assumptions about the machine it will run on: that PHP's
# opcode cache can be persuaded to notice a swapped release, that databases or
# at least tables can be created from SQL, that a long-lived process can be
# kept alive, that rename() is atomic between the store and the docroot. Every
# one of those is false on some hosting setup. This script checks them instead
# of guessing, and prints what it found.
#
# It reads. It does not write anything outside its own output file, does not
# need root, does not restart anything, and never prints a password. Read it
# before running it -- that is the point of shipping it as source.
#
# Usage:
#   ./recon.sh /path/to/wordpress            > recon-output.txt 2>&1
#
# Then paste the output back. If anything in it looks too revealing to share,
# cut it: everything here is a question, not a requirement.

set -u

# Server binaries live in sbin, which is not on an unprivileged user's PATH on
# Debian. Without this the whole web-server section comes back empty and reads
# as "no Apache installed", which is a much more alarming answer than the truth.
PATH="$PATH:/usr/sbin:/sbin:/usr/local/sbin"

WP_PATH="${1:-}"

say()  { printf '\n== %s ==\n' "$1"; }
item() { printf '%-34s %s\n' "$1" "$2"; }
have() { command -v "$1" >/dev/null 2>&1; }

# find_wordpress looks in the usual places. Passing the path is still better,
# but a run that silently skips two thirds of its checks because an argument was
# omitted is a bad way to find that out.
find_wordpress() {
	local c
	for c in \
		"$HOME/public_html" "$HOME/www" "$HOME/htdocs" \
		"$HOME"/*/public_html "$HOME"/*/httpdocs "$HOME"/*/htdocs \
		/var/www/html /var/www/*/public_html /var/www/*/htdocs /var/www/*
	do
		[ -r "$c/wp-config.php" ] && { echo "$c"; return 0; }
	done

	# Last resort: walk the home directory. Bounded depth so this stays quick.
	c="$(find "$HOME" -maxdepth 5 -name wp-config.php -readable 2>/dev/null | head -1)"
	[ -n "$c" ] && { dirname "$c"; return 0; }

	return 1
}

if [ -z "$WP_PATH" ]; then
	if WP_PATH="$(find_wordpress)"; then
		printf 'No path given; found WordPress at %s\n' "$WP_PATH"
	else
		WP_PATH=""
	fi
fi

# run_sql executes a query and prints the result, using whatever route works.
# wp-cli is preferred because it reads wp-config.php itself and no credential
# ever passes through this script.
run_sql() {
	local q="$1"

	if [ -n "${WP_CLI:-}" ]; then
		$WP_CLI db query "$q" --path="$WP_PATH" --skip-plugins --skip-themes 2>&1
		return
	fi

	if [ -n "${MYSQL_CNF:-}" ]; then
		mysql --defaults-extra-file="$MYSQL_CNF" --table -e "$q" 2>&1
		return
	fi

	echo "(no database route available)"
}

printf 'wpstaging recon -- %s on %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$(hostname 2>/dev/null || echo unknown)"

if [ -z "$WP_PATH" ] || [ ! -d "$WP_PATH" ]; then
	cat <<'WARN'

!! ---------------------------------------------------------------------- !!
!! No WordPress installation was given or found.                          !!
!!                                                                        !!
!! Everything that decides the design -- database privileges, which        !!
!! tables production writes, site size, whether the docroot can be         !!
!! swapped -- needs it. The run below will still print the host survey,    !!
!! but the important half will be missing.                                 !!
!!                                                                        !!
!!   ./recon.sh /path/to/wordpress                                         !!
!! ---------------------------------------------------------------------- !!

WARN
fi

# ---------------------------------------------------------------- the machine

say "Machine"
item "user"        "$(id -un 2>/dev/null) (uid $(id -u 2>/dev/null), groups: $(id -Gn 2>/dev/null))"
item "os"          "$(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME" || uname -s)"
item "kernel"      "$(uname -r 2>/dev/null)"
item "cpus"        "$(nproc 2>/dev/null || echo '?')"
item "memory"      "$(free -h 2>/dev/null | awk '/^Mem:/{print $2" total, "$7" available"}')"

# Root is assumed absent. What matters is precisely which privileged things are
# nonetheless reachable, because each one that is removes a workaround.
say "Privilege"
if sudo -n true 2>/dev/null; then
	item "passwordless sudo" "YES -- changes several design choices"
else
	item "passwordless sudo" "no"
fi
item "can write /etc/apache2"   "$( [ -w /etc/apache2 ] 2>/dev/null && echo yes || echo no )"
item "systemctl present"        "$(have systemctl && echo yes || echo no)"
if have systemctl; then
	item "systemd --user works"   "$(systemctl --user is-system-running 2>/dev/null || echo 'no (matters: how the daemon stays alive)')"
	item "lingering enabled"      "$(loginctl show-user "$(id -un)" -p Linger --value 2>/dev/null || echo '?')"
fi

# With no root and no user systemd, something else has to start the UI at boot
# and restart it if it dies. cron's @reboot is the usual unprivileged answer.
say "Keeping a process alive"
if have crontab; then
	# `crontab -l` exits non-zero for an empty crontab as well as for a denied
	# one, and reporting "may be denied" for an account that simply has no jobs
	# yet sends you chasing a restriction that was never there. The message
	# tells the two apart.
	cronout="$(crontab -l 2>&1)"
	case "$?:$cronout" in
		0:*)          item "crontab" "usable ($(printf '%s\n' "$cronout" | grep -cvE '^\s*(#|$)') entries)"
		              item "@reboot entries" "$(printf '%s\n' "$cronout" | grep -c '@reboot')" ;;
		*no\ crontab*) item "crontab" "usable, currently empty" ;;
		*)            item "crontab" "DENIED or failing: $(printf '%s' "$cronout" | head -1)" ;;
	esac
else
	item "crontab" "absent"
fi
item "tmux / screen" "$( { have tmux && echo -n 'tmux '; } ; { have screen && echo -n screen; } ; echo)"
item "nohup" "$(have nohup && echo present || echo absent)"
item "ulimit -u (max procs)" "$(ulimit -u 2>/dev/null)"

# A control panel usually means vhosts and databases are created through it
# rather than by hand, which is a different integration entirely.
say "Control panel"
for marker in /usr/local/psa:Plesk /usr/local/cpanel:cPanel /usr/local/ispconfig:ISPConfig /usr/local/directadmin:DirectAdmin /usr/local/CyberCP:CyberPanel; do
	path="${marker%%:*}"; name="${marker##*:}"
	[ -e "$path" ] && item "detected" "$name at $path"
done
item "panel scan" "complete"

# ------------------------------------------------------------------- the web

say "Web server"
for bin in apache2ctl apachectl httpd nginx; do
	have "$bin" && item "$bin" "$($bin -v 2>&1 | head -1)"
done
if have apache2ctl || have apachectl; then
	APACHECTL="$(have apache2ctl && echo apache2ctl || echo apachectl)"
	# Needed to put the Go UI behind Apache with TLS terminated there.
	mods="$($APACHECTL -M 2>/dev/null | awk '{print $1}' | tr '\n' ' ')"
	for m in proxy_module proxy_http_module ssl_module headers_module rewrite_module authn_file_module auth_basic_module md_module; do
		case " $mods " in
			*" $m "*) item "mod ${m%_module}" "present" ;;
			*)        item "mod ${m%_module}" "ABSENT or not listable unprivileged" ;;
		esac
	done
fi

# Who owns the vhosts decides how the two subdomains get created, and whether
# the Go UI can be proxied without asking someone with root.
say "Apache configuration"
for d in /etc/apache2/sites-enabled /etc/apache2/vhosts.d /etc/httpd/conf.d /etc/apache2/conf.d; do
	if [ -d "$d" ]; then
		item "$d" "$( [ -r "$d" ] && echo "readable, $(ls -1 "$d" 2>/dev/null | wc -l) files" || echo 'present but not readable' )"
		[ -r "$d" ] && ls -1 "$d" 2>/dev/null | sed 's/^/    /'
	fi
done
item "can write vhost dir" "$( { [ -w /etc/apache2/sites-enabled ] || [ -w /etc/httpd/conf.d ]; } 2>/dev/null && echo yes || echo 'no -- subdomains need the server admin' )"

# .htaccess is the unprivileged lever: if AllowOverride permits it, basic auth
# and the ACME exemption can be arranged without touching a vhost.
if [ -n "$WP_PATH" ] && [ -r "$WP_PATH/.htaccess" ]; then
	item ".htaccess" "present, $(wc -l < "$WP_PATH/.htaccess") lines"
fi

say "Listening ports"
if have ss; then
	ss -ltn 2>/dev/null | awk 'NR==1 || $4 ~ /:(80|443|3306|8[0-9]{3})$/'
elif have netstat; then
	netstat -ltn 2>/dev/null | head -20
else
	echo "(neither ss nor netstat available)"
fi

say "PHP"
if have php; then
	item "cli version" "$(php -v 2>/dev/null | head -1)"

	# The opcode cache is the single most important unknown for the release
	# swap. OPcache keys compiled scripts on the path as requested, so with a
	# docroot symlink the path never changes between releases and stale code is
	# served until something invalidates it. revalidate_path=1 fixes it
	# outright; otherwise the swap has to call opcache_reset() over HTTP, which
	# also flushes production's cache in a shared pool.
	php -r '
		$keys = ["opcache.enable","opcache.enable_cli","opcache.validate_timestamps",
		         "opcache.revalidate_freq","opcache.revalidate_path","opcache.restrict_api",
		         "realpath_cache_size","realpath_cache_ttl","memory_limit",
		         "max_execution_time","disable_functions","user_ini.filename"];
		foreach ($keys as $k) { printf("%-34s %s\n", $k, var_export(ini_get($k), true)); }
	' 2>/dev/null
	item "opcache_reset callable" "$(php -r 'echo function_exists("opcache_reset") ? "yes" : "no";' 2>/dev/null)"
else
	item "php cli" "ABSENT -- recon of PHP settings not possible from the shell"
fi

# The CLI's php.ini is often not the FPM pool's. This is the one that counts.
say "PHP as the web server runs it"
echo "The values above are the CLI's, which frequently differ from the FPM"
echo "pool's. To get the real ones, drop a file containing exactly:"
echo '    <?php phpinfo();'
echo "into the docroot, load it over HTTPS, note opcache.* and realpath_cache_*,"
echo "then DELETE IT. Do not leave it there -- it lists paths, versions and"
echo "extensions to anyone who asks."

# ------------------------------------------------------------- the filesystem

say "Filesystem"
if [ -n "$WP_PATH" ] && [ -d "$WP_PATH" ]; then
	item "wordpress path" "$WP_PATH"
	item "docroot is a symlink" "$( [ -L "$WP_PATH" ] && echo "yes -> $(readlink "$WP_PATH")" || echo no )"
	item "writable by me" "$( [ -w "$WP_PATH" ] && echo yes || echo 'NO -- deploys cannot write here' )"
	item "owner" "$(stat -c '%U:%G mode %a' "$WP_PATH" 2>/dev/null)"

	# rename() is atomic only within one filesystem, and the release swap and
	# the chunk store both depend on it.
	item "filesystem" "$(df -PTh "$WP_PATH" 2>/dev/null | awk 'NR==2{print $2" on "$1", "$4" used, "$5" free ("$6" full)"}')"
	item "parent filesystem" "$(df -PTh "$(dirname "$WP_PATH")" 2>/dev/null | awk 'NR==2{print $2" on "$1}')"
	item "home filesystem" "$(df -PTh "$HOME" 2>/dev/null | awk 'NR==2{print $2" on "$1", "$5" free"}')"

	say "Site size"
	echo "(counting; this walks the tree and may take a minute)"
	item "total" "$(du -sh "$WP_PATH" 2>/dev/null | cut -f1)"
	[ -d "$WP_PATH/wp-content/uploads" ] && \
		item "uploads" "$(du -sh "$WP_PATH/wp-content/uploads" 2>/dev/null | cut -f1), $(find "$WP_PATH/wp-content/uploads" -type f 2>/dev/null | wc -l) files"
	item "total files" "$(find "$WP_PATH" -type f 2>/dev/null | wc -l)"
	item "plugins" "$(ls -1 "$WP_PATH/wp-content/plugins" 2>/dev/null | wc -l)"
	item "themes" "$(ls -1 "$WP_PATH/wp-content/themes" 2>/dev/null | wc -l)"
else
	item "wordpress path" "NOT GIVEN OR NOT A DIRECTORY -- pass it as the first argument"
fi

# --------------------------------------------------------------- the database

say "Database access"
WP_CLI=""
if have wp; then
	WP_CLI="wp"
elif [ -x "$HOME/bin/wp" ]; then
	WP_CLI="$HOME/bin/wp"
fi

[ -z "$WP_CLI" ] && [ -x "$HOME/wp-cli.phar" ] && WP_CLI="php $HOME/wp-cli.phar"

if [ -n "$WP_CLI" ]; then
	item "wp-cli" "$($WP_CLI --version 2>/dev/null | head -1)"
else
	item "wp-cli" "absent -- it is a single phar and needs no privileges:"
	echo '        curl -sLO https://raw.githubusercontent.com/wp-cli/builds/gh-pages/phar/wp-cli.phar'
	echo '        php wp-cli.phar --info      # then re-run this script'
fi

MYSQL_CNF=""
if [ -z "$WP_CLI" ] && [ -n "$WP_PATH" ] && [ -r "$WP_PATH/wp-config.php" ] && have mysql; then
	# Fall back to reading wp-config.php directly. The password is written to a
	# 0600 file and passed to mysql by path; it is never echoed and the file is
	# removed on exit.
	cfgval() { sed -n "s/.*define(\s*['\"]$1['\"]\s*,\s*['\"]\(.*\)['\"]\s*).*/\1/p" "$WP_PATH/wp-config.php" | head -1; }

	MYSQL_CNF="$(mktemp)"
	chmod 600 "$MYSQL_CNF"
	trap 'rm -f "$MYSQL_CNF"' EXIT
	{
		echo "[client]"
		echo "user=$(cfgval DB_USER)"
		echo "password=$(cfgval DB_PASSWORD)"
		echo "database=$(cfgval DB_NAME)"
		host="$(cfgval DB_HOST)"
		case "$host" in
			*:*) echo "host=${host%%:*}"; echo "port=${host##*:}" ;;
			"")  ;;
			*)   echo "host=$host" ;;
		esac
	} > "$MYSQL_CNF"
	item "credentials" "read from wp-config.php into a temporary 0600 file"
fi

item "server version" "$(run_sql 'SELECT VERSION();' | tail -1)"
item "current user"   "$(run_sql 'SELECT CURRENT_USER();' | tail -1)"

# The census queries below name real tables, so the prefix has to be known
# first. A ten-year-old site installed before anyone worried about it may well
# not be using wp_.
PREFIX="wp_"
if [ -n "$WP_CLI" ]; then
	PREFIX="$($WP_CLI config get table_prefix --path="$WP_PATH" 2>/dev/null || echo wp_)"
elif [ -n "$WP_PATH" ] && [ -r "$WP_PATH/wp-config.php" ]; then
	PREFIX="$(sed -n "s/.*\$table_prefix\s*=\s*['\"]\(.*\)['\"].*/\1/p" "$WP_PATH/wp-config.php" | head -1)"
	PREFIX="${PREFIX:-wp_}"
fi
item "table prefix" "$PREFIX"

# The decisive question for the release model. With CREATE/DROP DATABASE, each
# release gets its own schema. Without, each release gets its own table prefix
# inside the one database -- same atomicity, fewer privileges.
say "Database privileges"
echo "Looking for CREATE, DROP, and whether they extend to whole databases."
echo "Password hashes are redacted: SHOW GRANTS prints IDENTIFIED BY PASSWORD,"
echo "and a mysql_native_password hash is SHA1(SHA1(password)) -- crackable"
echo "offline for anything short. It has no business in a file you paste around."
run_sql 'SHOW GRANTS FOR CURRENT_USER();' | sed -E "s/IDENTIFIED BY PASSWORD '[^']*'/IDENTIFIED BY PASSWORD '<redacted>'/g"

# ------------------------------------------------------ what production writes

say "Live-data census"
echo "Which tables accumulate rows on production decides which must be carried"
echo "over on promotion rather than taken from the snapshot. Comments are known"
echo "to; this is looking for the others."
echo

run_sql "
  SELECT table_name, engine, table_rows,
         ROUND((data_length+index_length)/1048576) AS mb,
         update_time
  FROM information_schema.tables
  WHERE table_schema = DATABASE()
  ORDER BY table_rows DESC;"

say "Comments"
run_sql "SELECT comment_approved AS status, COUNT(*) AS n FROM \`${PREFIX}comments\` GROUP BY comment_approved;"

say "Accumulation rate"
echo "How fast production gains rows decides how long a staging cycle can run"
echo "before promotion loses too much, and which tables have to be carried over"
echo "from live rather than taken from the snapshot."
run_sql "
  SELECT 'comments' AS what, COUNT(*) AS total,
         SUM(comment_date > NOW() - INTERVAL 30 DAY) AS last_30d,
         SUM(comment_date > NOW() - INTERVAL 365 DAY) AS last_year,
         MAX(comment_date) AS most_recent
  FROM \`${PREFIX}comments\`;"
run_sql "
  SELECT 'users' AS what, COUNT(*) AS total,
         SUM(user_registered > NOW() - INTERVAL 30 DAY) AS last_30d,
         MAX(user_registered) AS most_recent
  FROM \`${PREFIX}users\`;"
run_sql "
  SELECT post_type, post_status, COUNT(*) AS n, MAX(post_modified) AS most_recent
  FROM \`${PREFIX}posts\` GROUP BY post_type, post_status ORDER BY n DESC;"

say "Site"
if [ -n "$WP_CLI" ]; then
	item "wp version" "$($WP_CLI core version --path="$WP_PATH" 2>/dev/null)"
	item "multisite" "$($WP_CLI config get MULTISITE --path="$WP_PATH" 2>/dev/null || echo 'not set (single site)')"
	item "siteurl" "$($WP_CLI option get siteurl --path="$WP_PATH" --skip-plugins --skip-themes 2>/dev/null)"
	item "home" "$($WP_CLI option get home --path="$WP_PATH" --skip-plugins --skip-themes 2>/dev/null)"
	item "active plugins" "$($WP_CLI plugin list --status=active --field=name --path="$WP_PATH" 2>/dev/null | tr '\n' ' ')"
elif [ -n "$WP_PATH" ] && [ -r "$WP_PATH/wp-config.php" ]; then
	item "prefix" "$(sed -n "s/.*\$table_prefix\s*=\s*['\"]\(.*\)['\"].*/\1/p" "$WP_PATH/wp-config.php" | head -1)"
	item "wp version" "$(sed -n "s/.*\$wp_version\s*=\s*'\(.*\)'.*/\1/p" "$WP_PATH/wp-includes/version.php" 2>/dev/null | head -1)"
fi

say "Tooling"
for t in mysql mysqldump mariadb-dump rsync tar zstd curl git openssl certbot; do
	have "$t" && item "$t" "$(command -v "$t")" || item "$t" "absent"
done

say "Done"
echo "Nothing was modified. If you added a phpinfo() file for the section above,"
echo "delete it now."
