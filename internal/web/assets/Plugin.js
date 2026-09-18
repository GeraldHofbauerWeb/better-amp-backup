/* amp-bb, as a tab in AMP's own panel.
 *
 * AMP's PluginHandler fetches this file, evaluates it with new Function() and
 * calls the result as a constructor, so `this` is the module: it wants
 * `tabs`, optionally `stylesheet`, and a `plugin` with lifecycle hooks. That
 * is also why this is a plain statement sequence rather than an ES module,
 * and why it has to survive being evaluated twice.
 *
 * Everything below talks to /amp-bb/api, which nginx routes to the amp-bb
 * daemon on the same origin as the panel. Same origin is what makes the
 * session cookie work and CORS irrelevant.
 */

const AMPBB_API = '/amp-bb/api';
const AMPBB_TAB_ID = 'tab_AmpBB_ampbb';
const AMPBB_TAB = '#' + AMPBB_TAB_ID;

let csrf = '';
let caps = {};
let status = null;
let settings = null;
let defaults = { hot: [], excludes: [], retention: {} };
let snapshots = [];
let selectedSnapshot = null;
let stream = null;
let currentJob = null;
let refreshTimer = null;

/* The set of ticked paths, kept as a minimal cover: a fully ticked directory
 * is one entry, not thirty thousand. That is the contract the daemon expects
 * and the reason a world restore is one path rather than a request body the
 * size of the world. */
const selection = new Set();

let browsePath = '';
let browseOffset = 0;

/* --- talking to the daemon ------------------------------------------------ */

async function api(method, path, body) {
    const opts = {
        method,
        headers: { 'Accept': 'application/json' },
        credentials: 'same-origin',
    };
    if (body !== undefined) {
        opts.headers['Content-Type'] = 'application/json';
        opts.body = JSON.stringify(body);
    }
    if (method !== 'GET' && csrf) { opts.headers['X-AMPBB-CSRF'] = csrf; }

    const response = await fetch(AMPBB_API + path, opts);
    const text = await response.text();
    let payload = null;
    if (text) { try { payload = JSON.parse(text); } catch (e) { payload = null; } }

    if (!response.ok) {
        const err = new Error((payload && payload.error) || ('HTTP ' + response.status));
        err.status = response.status;
        err.payload = payload || {};
        throw err;
    }
    return payload;
}

async function signIn() {
    /* API.GetSessionID() is the authority: in an instance view it holds the
     * session AMP issued for that instance, while localStorage may still hold
     * the controller's. The fallback is for the moment before the panel has
     * finished wiring itself up. */
    const session = (typeof API !== 'undefined' && API.GetSessionID()) || localStorage['LastSessionID'] || '';
    const user = (typeof viewModels !== 'undefined' && viewModels.userinfo && viewModels.userinfo.username)
        ? viewModels.userinfo.username() : '';
    const result = await api('POST', '/session', { session: session, user: user });
    csrf = result.csrf;
    caps = result.capabilities || {};
    return result;
}

/* --- rendering helpers ---------------------------------------------------- */

const el = (id) => document.getElementById(id);

function text(node, value) { if (node) { node.textContent = value; } }

function bytes(n) {
    if (n === null || n === undefined) { return '—'; }
    if (n < 1024) { return n + ' B'; }
    const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
    let value = n / 1024, i = 0;
    while (value >= 1024 && i < units.length - 1) { value /= 1024; i++; }
    return value.toFixed(1) + ' ' + units[i];
}

/* Both zones, always. Working out which snapshot was "20:00 my time" from a
 * list of UTC stamps was the single most annoying part of the restore this
 * interface exists because of. */
function whenLocal(iso) {
    if (!iso) { return '—'; }
    const d = new Date(iso);
    if (isNaN(d)) { return iso; }
    return d.toLocaleString(undefined, {
        year: 'numeric', month: '2-digit', day: '2-digit',
        hour: '2-digit', minute: '2-digit', second: '2-digit',
    });
}

function whenUTC(iso) {
    if (!iso) { return ''; }
    const d = new Date(iso);
    if (isNaN(d)) { return ''; }
    return d.toISOString().replace('T', ' ').replace(/\.\d+Z$/, ' UTC');
}

function ago(iso, now) {
    if (!iso) { return ''; }
    const then = new Date(iso).getTime();
    if (isNaN(then)) { return ''; }
    const seconds = Math.round(((now ? new Date(now).getTime() : Date.now()) - then) / 1000);
    const future = seconds < 0;
    let n = Math.abs(seconds), unit = 'second';
    for (const [limit, name, div] of [[60, 'second', 1], [3600, 'minute', 60], [86400, 'hour', 3600], [Infinity, 'day', 86400]]) {
        if (n < limit) { n = Math.round(n / div); unit = name; break; }
    }
    const plural = n === 1 ? '' : 's';
    return future ? ('in ' + n + ' ' + unit + plural) : (n + ' ' + unit + plural + ' ago');
}

function clear(node) { while (node && node.firstChild) { node.removeChild(node.firstChild); } }

/* Go prints durations as "48h0m0s". Nobody wants to read that, and nobody
 * should have to type it either, so it is parsed into minutes on the way in
 * and written back in whichever unit the control used. */
function durationMinutes(text) {
    if (!text) { return 0; }
    let total = 0, matched = false;
    for (const [pattern, factor] of [[/(\d+(?:\.\d+)?)h/, 60], [/(\d+(?:\.\d+)?)m(?!s)/, 1], [/(\d+(?:\.\d+)?)s/, 1 / 60]]) {
        const m = pattern.exec(text);
        if (m) { total += parseFloat(m[1]) * factor; matched = true; }
    }
    return matched ? Math.round(total) : 0;
}

function minutesToDuration(minutes) {
    minutes = Math.max(0, Math.round(minutes || 0));
    if (minutes === 0) { return '0s'; }
    if (minutes % 60 === 0) { return (minutes / 60) + 'h'; }
    return minutes + 'm';
}

function make(tag, className, content) {
    const node = document.createElement(tag);
    if (className) { node.className = className; }
    if (content !== undefined) { node.textContent = content; }
    return node;
}

function icon(name) { return make('span', 'mat-icon', name); }

/* --- banners -------------------------------------------------------------- */

function banner(kind, title, detail, actions) {
    const node = make('div', 'ampbb-banner ampbb-banner-' + kind);
    node.appendChild(icon(kind === 'error' ? 'error' : kind === 'warn' ? 'warning' : 'info'));
    const body = make('div', 'ampbb-banner-body');
    body.appendChild(make('div', 'ampbb-banner-title', title));
    if (detail) { body.appendChild(make('div', '', detail)); }
    node.appendChild(body);
    if (actions && actions.length) {
        const bar = make('div', 'ampbb-banner-actions');
        actions.forEach((a) => {
            const button = make('button', 'slimButton', a.label);
            button.addEventListener('click', a.onClick);
            bar.appendChild(button);
        });
        node.appendChild(bar);
    }
    return node;
}

function renderBanners() {
    const host = el('ampbb-banners');
    if (!host || !status) { return; }
    clear(host);

    const state = status.instance.state;
    if (status.instance.error) {
        host.appendChild(banner('error', 'AMP could not be reached',
            status.instance.error + ' — the snapshots are unaffected and still restorable.'));
    } else if (state !== 'stopped') {
        const actions = [];
        if (caps.stop && state === 'ready') {
            actions.push({ label: 'Stop the server', onClick: () => confirmStop() });
        }
        host.appendChild(banner('info', 'The server is ' + state,
            'Restoring into the live instance needs it stopped. Browsing snapshots and restoring to a scratch copy work either way.',
            actions));
    } else if (caps.start) {
        host.appendChild(banner('info', 'The server is stopped',
            'Restoring into the live instance is possible now.',
            [{ label: 'Start the server', onClick: () => runInstance('start') }]));
    }

    if (status.amp_backups && status.amp_backups.enabled) {
        const which = (status.amp_backups.triggers || [])
            .map((t) => t.description).filter(Boolean).join(', ');
        host.appendChild(banner('warn', "AMP's own backup schedule is still running",
            'Both tools are backing up this instance' + (which ? ' (' + which + ')' : '') +
            '. AMP writes a complete archive every time, so that is where the disk is going. ' +
            "Switch it off under Schedule if amp-bb is doing the job."));
    }

    const last = status.last_backup;
    if (last && !last.success) {
        host.appendChild(banner('error', 'The last backup failed', last.error || ''));
    }
    if (status.last_skip) {
        host.appendChild(banner('warn', 'A scheduled backup was skipped',
            status.last_skip.reason + ' — ' + ago(status.last_skip.at, status.server_now) + '.'));
    }

    /* A banner appearing or going takes its height from whatever is below it,
     * and what is below it is the browser. */
    sizeTheBrowser();
}

/* --- overview ------------------------------------------------------------- */

function renderOverview() {
    if (!status) { return; }
    /* last_backup is only what this daemon did; last_snapshot is what is
     * actually in the repository. After migrating off the systemd timers the
     * first is empty and the second is not, and "never" over fifty snapshots
     * would be a lie told on a technicality. */
    const run = status.last_backup;
    const snap = status.last_snapshot;
    const when = (run && run.started_at) || (snap && snap.started_at) || null;
    text(el('ampbb-last-backup'), when ? ago(when, status.server_now) : 'never');
    if (!when) {
        text(el('ampbb-last-backup-sub'), 'no snapshot has been taken yet');
    } else {
        let sub = whenLocal(when) + '  ·  ' + whenUTC(when);
        if (run && !run.success) { sub += '  ·  failed'; }
        else if (!run && snap) { sub += '  ·  taken before this service took over'; }
        text(el('ampbb-last-backup-sub'), sub);
    }

    text(el('ampbb-next-backup'), status.next_backup ? ago(status.next_backup, status.server_now) : 'not scheduled');
    text(el('ampbb-next-backup-sub'), status.next_backup ? whenLocal(status.next_backup) : '');

    const stats = status.repo.stats || {};
    text(el('ampbb-repo-size'), bytes(stats.stored_bytes));
    const ratio = stats.stored_bytes > 0 ? (stats.logical_bytes / stats.stored_bytes) : 0;
    text(el('ampbb-repo-sub'), (stats.snapshots || 0) + ' snapshots, ' +
        (ratio ? ratio.toFixed(1) + '× saved' : 'nothing stored yet') +
        (status.repo.stats_age_seconds > 90 ? ' (as of ' + Math.round(status.repo.stats_age_seconds / 60) + ' min ago)' : ''));

    const space = status.repo.space || {};
    text(el('ampbb-disk'), bytes(space.available_bytes));
    const usedPct = space.total_bytes ? Math.round(100 * (1 - space.available_bytes / space.total_bytes)) : 0;
    text(el('ampbb-disk-sub'), space.total_bytes ? usedPct + '% of ' + bytes(space.total_bytes) + ' used' : '');

    for (const [id, cap] of [['ampbb-run-backup', 'backup'], ['ampbb-run-check', 'read'], ['ampbb-run-housekeeping', 'destroy']]) {
        const button = el(id);
        if (button) { button.disabled = !caps[cap]; }
    }
}

/* --- jobs ----------------------------------------------------------------- */

function renderJob(job, logLines) {
    const host = el('ampbb-job');
    if (!host) { return; }
    currentJob = job;
    if (!job) { host.hidden = true; return; }

    host.hidden = false;
    text(el('ampbb-job-title'), job.kind + ' — ' + job.state +
        (job.started_by ? ' (' + job.started_by + ')' : ''));
    const pct = job.total > 0 ? Math.min(100, Math.round(100 * job.done / job.total)) : (job.state === 'running' ? 0 : 100);
    const bar = el('ampbb-job-bar');
    if (bar) { bar.style.width = pct + '%'; }
    text(el('ampbb-job-stage'), job.stage
        ? job.stage + (job.total > 0 ? '  ' + job.done + ' / ' + job.total : '')
        : (job.error || ''));

    const cancel = el('ampbb-job-cancel');
    if (cancel) {
        cancel.hidden = job.state !== 'running';
        cancel.disabled = !!job.cancelling;
        cancel.textContent = job.kind === 'restore'
            ? 'Abort (leaves the instance half-restored)'
            : (job.cancelling ? 'Cancelling…' : 'Cancel');
    }
    if (logLines) {
        const log = el('ampbb-job-log');
        if (log) {
            log.textContent = logLines.map((l) => l.text).join('\n');
            log.scrollTop = log.scrollHeight;
        }
    }
}

function appendLog(line) {
    const log = el('ampbb-job-log');
    if (!log) { return; }
    log.textContent += (log.textContent ? '\n' : '') + line.text;
    log.scrollTop = log.scrollHeight;
}

function openStream() {
    if (stream) { stream.close(); }
    stream = new EventSource(AMPBB_API + '/events');
    stream.addEventListener('job', (ev) => {
        const data = JSON.parse(ev.data);
        renderJob(data.job);
        if (data.job && data.job.state !== 'running') {
            /* A finished job changes the numbers on the overview and may have
             * added a snapshot. */
            refresh();
        }
    });
    stream.addEventListener('progress', (ev) => renderJob(JSON.parse(ev.data).job));
    stream.addEventListener('log', (ev) => appendLog(JSON.parse(ev.data).log));
    stream.onerror = () => { /* EventSource reconnects on its own, with Last-Event-ID. */ };
}

/* --- snapshots ------------------------------------------------------------ */

function renderSnapshots() {
    const list = el('ampbb-snapshot-list');
    if (!list) { return; }
    clear(list);
    text(el('ampbb-snapshot-count'), snapshots.length + ' kept');

    snapshots.forEach((m) => {
        const row = make('button', 'ampbb-snapshot' +
            (selectedSnapshot && selectedSnapshot.id === m.id ? ' ampbb-snapshot-selected' : ''));
        const when = make('span', 'ampbb-snapshot-when', whenLocal(m.started_at));
        if (m.state === 'partial') { when.appendChild(make('span', 'ampbb-pill ampbb-pill-partial', 'partial')); }
        (m.tags || []).forEach((tag) => when.appendChild(make('span', 'ampbb-pill ampbb-pill-tag', tag)));
        row.appendChild(when);
        row.appendChild(make('span', 'ampbb-snapshot-meta',
            whenUTC(m.started_at) + '  ·  ' + (m.stats.files || 0) + ' files  ·  ' +
            bytes(m.stats.total_bytes) + ' described, ' + bytes(m.stats.new_bytes) + ' added'));
        row.addEventListener('click', () => selectSnapshot(m));
        list.appendChild(row);
    });
}

async function selectSnapshot(m) {
    selectedSnapshot = m;
    selection.clear();
    browsePath = '';
    browseOffset = 0;
    renderSnapshots();
    const search = el('ampbb-search');
    if (search) { search.disabled = false; search.value = ''; }
    text(el('ampbb-browser-title'), whenLocal(m.started_at) + '  ·  ' + m.id);
    await browse('');
    renderSelection();
}

/* --- the file browser ----------------------------------------------------- */

function coveredBySelection(path) {
    if (selection.has(path)) { return true; }
    for (const chosen of selection) {
        if (path.startsWith(chosen + '/')) { return true; }
    }
    return false;
}

/* Ticking a directory replaces everything already ticked underneath it, so the
 * set stays a minimal cover however the boxes were clicked. */
function select(path) {
    for (const chosen of Array.from(selection)) {
        if (chosen === path || chosen.startsWith(path + '/')) { selection.delete(chosen); }
    }
    selection.add(path);
}

/* Unticking something inside a ticked parent: the parent has to be replaced by
 * its other children, recursively, so that the selection stays a cover of
 * exactly what is still ticked. This is why the set is the minimal cover and
 * not simply every path -- the expansion only happens where it has to. */
async function deselect(path) {
    if (selection.delete(path)) { return; }

    let parent = null;
    for (const chosen of selection) {
        if (path.startsWith(chosen + '/')) { parent = chosen; break; }
    }
    if (parent === null) { return; }

    selection.delete(parent);
    await expandAround(parent, path);
}

/* Replace `parent` in the selection with its children, leaving out the branch
 * that leads to `exclude`, and descend into that branch to do the same. */
async function expandAround(parent, exclude) {
    const listing = await api('GET', '/snapshots/' + encodeURIComponent(selectedSnapshot.id) +
        '/tree?path=' + encodeURIComponent(parent) + '&limit=5000');

    for (const node of (listing.entries || [])) {
        if (node.path === exclude) { continue; }
        if (exclude.startsWith(node.path + '/')) {
            // The branch the unticked path lives in: keep descending.
            await expandAround(node.path, exclude);
            continue;
        }
        selection.add(node.path);
    }
}

async function browse(path, offset) {
    if (!selectedSnapshot) { return; }
    browsePath = path || '';
    browseOffset = offset || 0;
    const listing = await api('GET', '/snapshots/' + encodeURIComponent(selectedSnapshot.id) +
        '/tree?path=' + encodeURIComponent(browsePath) + '&offset=' + browseOffset + '&limit=200');
    renderCrumbs();
    renderTree(listing, browseOffset > 0);
}

function renderCrumbs() {
    const host = el('ampbb-crumbs');
    if (!host) { return; }
    clear(host);
    const parts = browsePath ? browsePath.split('/') : [];
    const root = make('button', 'ampbb-crumb', 'instance root');
    root.addEventListener('click', () => browse(''));
    host.appendChild(root);
    let acc = '';
    parts.forEach((part) => {
        acc = acc ? acc + '/' + part : part;
        const here = acc;
        host.appendChild(make('span', 'ampbb-crumb-sep', '/'));
        const crumb = make('button', 'ampbb-crumb', part);
        crumb.addEventListener('click', () => browse(here));
        host.appendChild(crumb);
    });
}

function renderTree(listing, append) {
    const host = el('ampbb-tree');
    if (!host) { return; }
    if (!append) { clear(host); }

    (listing.entries || []).forEach((node) => {
        const row = make('div', 'ampbb-row' + (node.type === 'd' ? ' ampbb-row-dir' : ''));

        const box = document.createElement('input');
        box.type = 'checkbox';
        box.checked = coveredBySelection(node.path);
        box.disabled = !caps.restore;
        box.addEventListener('change', async () => {
            // Both paths touch the network, so the box is disabled until the
            // tree has been redrawn from the new selection. Otherwise a quick
            // second click acts on a listing that is about to be replaced.
            box.disabled = true;
            try {
                if (box.checked) { select(node.path); } else { await deselect(node.path); }
                renderSelection();
                await browse(browsePath);
            } catch (err) {
                box.checked = !box.checked;
                box.disabled = false;
                throw err;
            }
        });
        row.appendChild(box);

        row.appendChild(icon(node.type === 'd' ? 'folder' : node.type === 'l' ? 'link' : 'description'));

        const name = make('button', 'ampbb-name', node.name);
        if (node.type === 'd') {
            name.addEventListener('click', () => browse(node.path));
        } else if (node.type === 'f') {
            name.style.cursor = 'pointer';
            name.title = 'Download this file out of the snapshot';
            name.addEventListener('click', () => {
                window.open(AMPBB_API + '/snapshots/' + encodeURIComponent(selectedSnapshot.id) +
                    '/file?path=' + encodeURIComponent(node.path), '_blank');
            });
        }
        row.appendChild(name);

        row.appendChild(make('span', 'ampbb-size', node.type === 'd'
            ? (node.children || 0) + ' files, ' + bytes(node.sub_bytes || 0)
            : bytes(node.size || 0)));
        host.appendChild(row);
    });

    const shown = (append ? browseOffset : 0) + (listing.entries || []).length;
    if (shown < listing.total) {
        const more = make('button', 'ampbb-more',
            'Show more (' + shown + ' of ' + listing.total + ')');
        more.addEventListener('click', async () => {
            more.remove();
            const next = await api('GET', '/snapshots/' + encodeURIComponent(selectedSnapshot.id) +
                '/tree?path=' + encodeURIComponent(browsePath) + '&offset=' + shown + '&limit=200');
            browseOffset = shown;
            renderTree(next, true);
        });
        host.appendChild(more);
    }
}

function renderSelection() {
    const bar = el('ampbb-restore-bar');
    if (!bar) { return; }
    bar.hidden = selection.size === 0;
    text(el('ampbb-selection-summary'), selection.size === 1
        ? '1 path selected'
        : selection.size + ' paths selected');
    const button = el('ampbb-restore');
    if (button) { button.disabled = !caps.restore; }
}

/* --- dialogs -------------------------------------------------------------- */

function dialog(title, buildBody, confirmLabel, onConfirm) {
    const backdrop = make('div', 'ampbb-dialog-backdrop');
    const box = make('div', 'ampbb-dialog');
    box.appendChild(make('h3', '', title));
    const body = make('div');
    buildBody(body);
    box.appendChild(body);

    const actions = make('div', 'ampbb-dialog-actions');
    const cancel = make('button', 'slimButton', 'Cancel');
    cancel.addEventListener('click', () => backdrop.remove());
    const confirm = make('button', 'button', confirmLabel);
    confirm.addEventListener('click', async () => {
        confirm.disabled = true;
        try { await onConfirm(body); backdrop.remove(); }
        catch (e) { confirm.disabled = false; showError(body, e); }
    });
    actions.appendChild(cancel);
    actions.appendChild(confirm);
    box.appendChild(actions);

    backdrop.appendChild(box);
    backdrop.addEventListener('click', (ev) => { if (ev.target === backdrop) { backdrop.remove(); } });
    document.body.appendChild(backdrop);
    return backdrop;
}

function showError(host, err) {
    const existing = host.querySelector('.ampbb-dialog-error');
    if (existing) { existing.remove(); }
    const detail = (err.payload && err.payload.detail) ? ' — ' + err.payload.detail : '';
    const node = make('div', 'ampbb-dialog-error ampbb-banner ampbb-banner-error', err.message + detail);
    host.appendChild(node);
}

function restoreDialog() {
    const paths = Array.from(selection).sort();
    dialog('Restore ' + (paths.length === 1 ? '1 path' : paths.length + ' paths'), (body) => {
        const list = make('div', 'ampbb-dialog-list');
        paths.slice(0, 50).forEach((p) => list.appendChild(make('div', '', p)));
        if (paths.length > 50) { list.appendChild(make('div', '', '… and ' + (paths.length - 50) + ' more')); }
        body.appendChild(list);

        const scratch = make('label');
        scratch.innerHTML = '<input type="radio" name="ampbb-target" value="scratch" checked> ' +
            'Into a scratch copy on the server — nothing live is touched';
        body.appendChild(scratch);

        const live = make('label');
        const stopped = status && status.instance.state === 'stopped';
        live.innerHTML = '<input type="radio" name="ampbb-target" value="instance"' +
            (stopped ? '' : ' disabled') + '> Into the live instance' +
            (stopped ? '' : ' — needs the server stopped');
        body.appendChild(live);

        const pre = make('label');
        pre.innerHTML = '<input type="checkbox" id="ampbb-pre-snapshot" checked> ' +
            'Take a snapshot of the current state first (tagged pre-restore, never forgotten)';
        body.appendChild(pre);

        const dry = make('label');
        dry.innerHTML = '<input type="checkbox" id="ampbb-dry-run"> Dry run: report what would happen and stop';
        body.appendChild(dry);
    }, 'Restore', async (body) => {
        const target = body.querySelector('input[name="ampbb-target"]:checked').value;
        await api('POST', '/jobs/restore', {
            snapshot: selectedSnapshot.id,
            paths: paths,
            target: target,
            pre_snapshot: body.querySelector('#ampbb-pre-snapshot').checked,
            dry_run: body.querySelector('#ampbb-dry-run').checked,
        });
        switchView('overview');
    });
}

function confirmStop() {
    dialog('Stop the server?', (body) => {
        body.appendChild(make('p', '',
            'Everyone on it will be disconnected. amp-bb stops it under your own AMP account, ' +
            'not under the backup service account — that one deliberately cannot.'));
    }, 'Stop it', async () => { await runInstance('stop'); });
}

async function runInstance(which) {
    await api('POST', '/instance/' + which, {});
    switchView('overview');
}

/* --- settings ------------------------------------------------------------- */

/* Every row of "how many backups to keep" is a rule that applies on its own,
 * and each one can be switched off without touching the others. Off is stored
 * as zero, because zero is already what repo.Policy means by "this rule does
 * not apply" -- the file keeps no second opinion about which rules are live,
 * and an operator reading settings.json by hand sees the same thing the tab
 * shows.
 *
 * The number a switched-off rule had stays in the box, greyed, so that ticking
 * it again restores what was there instead of quietly meaning "keep 0". */
const RETENTION_RULES = [
    { toggle: 'ampbb-keep-last-on', input: 'ampbb-keep-last' },
    { toggle: 'ampbb-keep-hourly-on', input: 'ampbb-keep-hourly' },
    { toggle: 'ampbb-keep-daily-on', input: 'ampbb-keep-daily' },
    { toggle: 'ampbb-keep-weekly-on', input: 'ampbb-keep-weekly' },
    { toggle: 'ampbb-keep-monthly-on', input: 'ampbb-keep-monthly' },
    { toggle: 'ampbb-keep-yearly-on', input: 'ampbb-keep-yearly' },
    { toggle: 'ampbb-within-on', input: 'ampbb-within-amount', also: ['ampbb-within-unit'] },
    { toggle: 'ampbb-min-snapshots-on', input: 'ampbb-min-snapshots' },
    { toggle: 'ampbb-keep-tags-on', input: 'ampbb-keep-tags' },
];

function ruleIsOn(toggleId) {
    const node = el(toggleId);
    return node ? node.checked : true;
}

/* Greying a row is not decoration: an unticked rule still shows a number, and
 * a number that is not in force has to look like one. */
function syncRetentionRows() {
    const editable = !!caps.settings;
    RETENTION_RULES.forEach((rule) => {
        const toggle = el(rule.toggle);
        const input = el(rule.input);
        if (!toggle || !input) { return; }
        const on = toggle.checked;
        toggle.disabled = !editable;
        [input].concat((rule.also || []).map(el)).forEach((node) => {
            if (node) { node.disabled = !editable || !on; }
        });
        const row = toggle.closest('.ampbb-row-setting');
        if (row) { row.classList.toggle('ampbb-rule-off', !on); }
    });
}

function renderSettings() {
    if (!settings) { return; }
    const s = settings.schedule || {};
    const h = s.housekeeping || {};
    const r = settings.retention || {};
    const x = settings.exclusions || {};

    const set = (id, value) => { const n = el(id); if (n) { n.value = value === undefined || value === null ? '' : value; } };
    const check = (id, value) => { const n = el(id); if (n) { n.checked = !!value; } };

    check('ampbb-schedule-enabled', s.enabled);
    check('ampbb-quiesce', s.quiesce);
    showInterval(s.every);
    set('ampbb-jitter', durationMinutes(s.jitter));
    set('ampbb-grace', durationMinutes(s.startup_grace));

    check('ampbb-house-enabled', h.enabled);
    set('ampbb-house-at', h.at);
    /* An empty zone means the server's own, which is not a useful thing to
     * show somebody. Offer theirs as a starting point instead. */
    set('ampbb-house-tz', h.tz || guessTimeZone());

    /* A rule is on when it has a value, off when it has none. The box then
     * shows the default rather than a bare zero, so that ticking it has an
     * obvious meaning. */
    const d = defaults.retention || {};
    const count = (id, toggleId, value, fallback) => {
        const on = (value || 0) > 0;
        check(toggleId, on);
        set(id, on ? value : (fallback || 0));
    };
    count('ampbb-keep-last', 'ampbb-keep-last-on', r.keep_last, d.keep_last);
    count('ampbb-keep-hourly', 'ampbb-keep-hourly-on', r.keep_hourly, d.keep_hourly);
    count('ampbb-keep-daily', 'ampbb-keep-daily-on', r.keep_daily, d.keep_daily);
    count('ampbb-keep-weekly', 'ampbb-keep-weekly-on', r.keep_weekly, d.keep_weekly);
    count('ampbb-keep-monthly', 'ampbb-keep-monthly-on', r.keep_monthly, d.keep_monthly);
    count('ampbb-keep-yearly', 'ampbb-keep-yearly-on', r.keep_yearly, d.keep_yearly);
    count('ampbb-min-snapshots', 'ampbb-min-snapshots-on', r.min_snapshots, d.min_snapshots);

    const withinMinutes = durationMinutes(r.keep_within);
    check('ampbb-within-on', withinMinutes > 0);
    showWithin(withinMinutes > 0 ? withinMinutes : durationMinutes(d.keep_within));

    const tags = r.keep_tags || [];
    check('ampbb-keep-tags-on', tags.length > 0);
    set('ampbb-keep-tags', (tags.length ? tags : (d.keep_tags || [])).join(', '));

    set('ampbb-exclusions', (x.patterns || []).join('\n'));
    check('ampbb-use-defaults', x.use_defaults);
    check('ampbb-honour-amp', x.honour_amp);

    const list = el('ampbb-default-list');
    if (list) {
        clear(list);
        defaults.excludes.forEach((pattern) => {
            const row = make('div');
            row.appendChild(make('code', '', pattern));
            list.appendChild(row);
        });
    }

    const editable = !!caps.settings;
    document.querySelectorAll('.ampbb-view[data-view="settings"] input, ' +
        '.ampbb-view[data-view="settings"] textarea, ' +
        '.ampbb-view[data-view="settings"] select, #ampbb-save-settings')
        .forEach((node) => { node.disabled = !editable; });
    /* After the sweep, not before: it would otherwise re-enable the boxes of
     * rules that are switched off. */
    syncRetentionRows();
    if (!editable) {
        text(el('ampbb-settings-status'), 'Read-only: this AMP account may not change the schedule.');
    }
}

function guessTimeZone() {
    try { return Intl.DateTimeFormat().resolvedOptions().timeZone || ''; }
    catch (e) { return ''; }
}

/* The interval is a dropdown of the answers people actually want, with a text
 * box for the ones they do not. */
function showInterval(every) {
    const preset = el('ampbb-every-preset');
    const custom = el('ampbb-every');
    if (!preset || !custom) { return; }
    const known = Array.from(preset.options).map((o) => o.value);
    const canonical = minutesToDuration(durationMinutes(every));
    if (known.indexOf(canonical) >= 0) {
        preset.value = canonical;
        custom.hidden = true;
        custom.value = '';
    } else {
        preset.value = 'custom';
        custom.hidden = false;
        custom.value = every || '';
    }
    describeInterval();
}

function describeInterval() {
    const note = el('ampbb-every-note');
    if (!note) { return; }
    const minutes = currentIntervalMinutes();
    if (!minutes) { text(note, ''); return; }
    const perDay = Math.round((24 * 60) / minutes);
    text(note, perDay >= 1 ? 'About ' + perDay + ' backup' + (perDay === 1 ? '' : 's') + ' a day.' : '');
}

function currentIntervalMinutes() {
    return durationMinutes(currentInterval());
}

function currentInterval() {
    const preset = el('ampbb-every-preset');
    const custom = el('ampbb-every');
    if (preset && preset.value !== 'custom') { return preset.value; }
    return custom ? custom.value.trim() : '';
}

/* Hours below a couple of days, days above -- which is how somebody would
 * actually say it. */
function showWithin(minutes) {
    const amount = el('ampbb-within-amount');
    const unit = el('ampbb-within-unit');
    if (!amount || !unit) { return; }
    const hours = Math.round(minutes / 60);
    if (hours >= 48 && hours % 24 === 0) {
        unit.value = 'd';
        amount.value = hours / 24;
    } else {
        unit.value = 'h';
        amount.value = hours;
    }
}

function withinDuration() {
    if (!ruleIsOn('ampbb-within-on')) { return '0s'; }
    const amount = parseInt((el('ampbb-within-amount') || {}).value, 10) || 0;
    const unit = (el('ampbb-within-unit') || {}).value;
    return (unit === 'd' ? amount * 24 : amount) + 'h';
}

/* The retention half of the settings, read straight off the page. It is its
 * own function because the preview asks for it on every keystroke, long
 * before anything is saved. */
function collectRetention() {
    const number = (id) => { const n = el(id); return n ? (parseInt(n.value, 10) || 0) : 0; };
    const ruled = (toggle, id) => (ruleIsOn(toggle) ? number(id) : 0);
    const tags = ruleIsOn('ampbb-keep-tags-on')
        ? ((el('ampbb-keep-tags') || {}).value || '').split(',').map((t) => t.trim()).filter(Boolean)
        : [];

    return {
        keep_last: ruled('ampbb-keep-last-on', 'ampbb-keep-last'),
        keep_hourly: ruled('ampbb-keep-hourly-on', 'ampbb-keep-hourly'),
        keep_daily: ruled('ampbb-keep-daily-on', 'ampbb-keep-daily'),
        keep_weekly: ruled('ampbb-keep-weekly-on', 'ampbb-keep-weekly'),
        keep_monthly: ruled('ampbb-keep-monthly-on', 'ampbb-keep-monthly'),
        keep_yearly: ruled('ampbb-keep-yearly-on', 'ampbb-keep-yearly'),
        keep_within: withinDuration(),
        keep_tags: tags,
        min_snapshots: ruled('ampbb-min-snapshots-on', 'ampbb-min-snapshots'),
    };
}

function collectSettings() {
    const value = (id) => { const n = el(id); return n ? n.value.trim() : ''; };
    const number = (id) => { const n = el(id); return n ? (parseInt(n.value, 10) || 0) : 0; };
    const checked = (id) => { const n = el(id); return n ? n.checked : false; };

    return {
        version: settings.version,
        instance: settings.instance,
        schedule: {
            enabled: checked('ampbb-schedule-enabled'),
            every: currentInterval(),
            jitter: minutesToDuration(number('ampbb-jitter')),
            quiesce: checked('ampbb-quiesce'),
            startup_grace: minutesToDuration(number('ampbb-grace')),
            housekeeping: {
                enabled: checked('ampbb-house-enabled'),
                at: value('ampbb-house-at'),
                tz: value('ampbb-house-tz'),
                check: true, forget: true, prune: true, empty_trash: true,
            },
        },
        retention: collectRetention(),
        exclusions: {
            use_defaults: checked('ampbb-use-defaults'),
            honour_amp: checked('ampbb-honour-amp'),
            patterns: value('ampbb-exclusions').split('\n').map((l) => l.trim()).filter(Boolean),
            hot: (settings.exclusions || {}).hot || [],
        },
        updated_at: settings.updated_at,
        updated_by: settings.updated_by,
    };
}

async function previewExclusions() {
    const host = el('ampbb-exclusion-preview');
    if (!host || !caps.settings) { return; }
    const node = el('ampbb-exclusions');
    const patterns = node ? node.value.split('\n').map((l) => l.trim()).filter(Boolean) : [];
    try {
        const preview = await api('POST', '/settings/validate', {
            patterns: patterns, use_defaults: el('ampbb-use-defaults').checked,
        });
        if (!preview.snapshot) { text(host, 'No snapshot to measure against yet.'); return; }
        text(host, 'Against ' + preview.snapshot + ': this leaves out ' + preview.excluded +
            ' of ' + preview.entries + ' entries, ' + bytes(preview.bytes) + '.' +
            (preview.samples && preview.samples.length ? ' For example ' + preview.samples.slice(0, 3).join(', ') + '.' : ''));
        host.className = 'ampbb-preview';
    } catch (e) {
        text(host, e.message);
        host.className = 'ampbb-preview ampbb-banner-error';
    }
}

/* The preview measures the rules as they stand on screen, not as they were
 * saved. That is what makes switching a rule off an informed decision rather
 * than a guess: the cost appears while the box is still under the cursor. */
async function loadRetentionPreview() {
    const host = el('ampbb-retention-preview');
    if (!host || !caps.read) { return; }
    try {
        const preview = settings
            ? await api('POST', '/retention/preview', collectRetention())
            : await api('GET', '/retention/preview');
        host.className = 'ampbb-preview';
        if (preview.would_forget === 0) {
            text(host, 'These rules keep all ' + preview.snapshots + ' snapshot(s). Nothing would be forgotten.');
            return;
        }
        text(host, 'These rules would forget ' + preview.would_forget + ' of ' +
            preview.snapshots + ' snapshot(s) at the next housekeeping run, keeping ' +
            preview.would_keep + '.');
        /* Losing most of the repository in one pass is a thing somebody may
         * well mean, and a thing nobody should do without noticing. */
        if (preview.would_forget > preview.would_keep) {
            host.className = 'ampbb-preview ampbb-banner-error';
        }
    } catch (e) {
        text(host, e.message);
        host.className = 'ampbb-preview ampbb-banner-error';
    }
}

/* --- views ---------------------------------------------------------------- */

function switchView(name) {
    document.querySelectorAll(AMPBB_TAB + ' .ampbb-view').forEach((node) => {
        node.hidden = node.dataset.view !== name;
    });
    document.querySelectorAll(AMPBB_TAB + ' .tabHeader').forEach((node) => {
        node.classList.toggle('active', node.dataset.view === name);
        node.setAttribute('aria-selected', node.dataset.view === name ? 'true' : 'false');
    });
    if (name === 'settings') { loadRetentionPreview(); previewExclusions(); }
    if (name === 'snapshots') { sizeTheBrowser(); }
}

/* The snapshot browser takes whatever is left of the window below it.
 *
 * How much that is cannot be written into the stylesheet: above it sit AMP's
 * own header, our banners -- which come and go -- and the tab strip, and none
 * of those has a height we may assume. So it is measured. The stylesheet keeps
 * a floor for the moments before the first measurement, and for the case where
 * this never runs at all. */
function sizeTheBrowser() {
    const split = document.querySelector(AMPBB_TAB + ' .ampbb-split');
    if (!split || split.offsetParent === null) { return; }
    const top = split.getBoundingClientRect().top;
    const room = window.innerHeight - top - 24;
    split.style.setProperty('--ampbb-split-height', Math.max(0, Math.round(room)) + 'px');
}

/* --- wiring --------------------------------------------------------------- */

async function refresh() {
    status = await api('GET', '/status');
    caps = status.capabilities || caps;
    renderBanners();
    renderOverview();
    const list = await api('GET', '/snapshots');
    snapshots = list.snapshots || [];
    renderSnapshots();
    if (!currentJob || currentJob.state !== 'running') {
        const jobs = await api('GET', '/jobs');
        const running = (jobs.jobs || []).find((j) => j.state === 'running');
        if (running) {
            const detail = await api('GET', '/jobs/' + encodeURIComponent(running.id));
            renderJob(detail.job, detail.log);
        }
    }
}

async function runJob(path, body) {
    try {
        await api('POST', path, body || {});
        switchView('overview');
    } catch (e) {
        if (e.status === 409 && e.payload && e.payload.job) {
            renderJob(e.payload.job);
            switchView('overview');
            return;
        }
        throw e;
    }
}

function wire() {
    // AMP's tab headers are divs, so they need the keyboard wired up by hand.
    document.querySelectorAll(AMPBB_TAB + ' .tabHeader').forEach((node) => {
        node.addEventListener('click', () => switchView(node.dataset.view));
        node.addEventListener('keydown', (ev) => {
            if (ev.key === 'Enter' || ev.key === ' ') {
                ev.preventDefault();
                switchView(node.dataset.view);
            }
        });
    });

    el('ampbb-run-backup').addEventListener('click', () => runJob('/jobs/backup', { tags: ['manual'] }));
    el('ampbb-run-check').addEventListener('click', () => runJob('/jobs/check', { read_data: false }));
    el('ampbb-run-housekeeping').addEventListener('click', () => {
        dialog('Run housekeeping now?', (body) => {
            body.appendChild(make('p', '',
                'Checks the repository, applies the retention policy, then reclaims the space. ' +
                'It stops before deleting anything if the check finds a problem.'));
            const dry = make('label');
            dry.innerHTML = '<input type="checkbox" id="ampbb-house-dry" checked> Report only, delete nothing';
            body.appendChild(dry);
        }, 'Run it', async (body) => {
            await runJob('/jobs/housekeeping', {
                forget: true, prune: true, empty_trash: true,
                dry_run: body.querySelector('#ampbb-house-dry').checked,
            });
        });
    });

    el('ampbb-job-cancel').addEventListener('click', () => {
        if (currentJob) { api('POST', '/jobs/' + encodeURIComponent(currentJob.id) + '/cancel', {}); }
    });

    window.addEventListener('resize', sizeTheBrowser);

    el('ampbb-restore').addEventListener('click', restoreDialog);
    el('ampbb-clear-selection').addEventListener('click', () => {
        selection.clear();
        renderSelection();
        browse(browsePath);
    });

    const search = el('ampbb-search');
    let searchTimer = null;
    search.addEventListener('input', () => {
        clearTimeout(searchTimer);
        searchTimer = setTimeout(async () => {
            if (!selectedSnapshot) { return; }
            const q = search.value.trim();
            if (!q) { await browse(browsePath); return; }
            const hits = await api('GET', '/snapshots/' + encodeURIComponent(selectedSnapshot.id) +
                '/search?q=' + encodeURIComponent(q));
            renderTree({ entries: hits.results || [], total: (hits.results || []).length }, false);
        }, 250);
    });

    el('ampbb-save-settings').addEventListener('click', async () => {
        const statusNode = el('ampbb-settings-status');
        try {
            const result = await api('PUT', '/settings', collectSettings());
            settings = result.settings;
            renderSettings();
            text(statusNode, 'Saved. ' + (settings.updated_by ? 'By ' + settings.updated_by + '.' : ''));
            loadRetentionPreview();
        } catch (e) {
            text(statusNode, e.message);
        }
    });

    el('ampbb-every-preset').addEventListener('change', () => {
        const custom = el('ampbb-every');
        custom.hidden = el('ampbb-every-preset').value !== 'custom';
        if (!custom.hidden && !custom.value) { custom.focus(); }
        describeInterval();
    });
    el('ampbb-every').addEventListener('input', describeInterval);

    /* The retention preview is what makes those numbers mean anything, so it
     * follows every change rather than waiting for a save. */
    let retentionTimer = null;
    const previewSoon = () => {
        clearTimeout(retentionTimer);
        retentionTimer = setTimeout(loadRetentionPreview, 400);
    };
    document.querySelectorAll('.ampbb-view[data-view="settings"] input[type="number"], ' +
        '#ampbb-keep-tags, #ampbb-within-unit')
        .forEach((node) => { node.addEventListener('input', previewSoon); });
    el('ampbb-within-unit').addEventListener('change', previewSoon);

    /* Switching a rule on or off is a bigger change than typing a digit, so it
     * asks straight away rather than waiting out the debounce. */
    RETENTION_RULES.forEach((rule) => {
        const toggle = el(rule.toggle);
        if (!toggle) { return; }
        toggle.addEventListener('change', () => {
            syncRetentionRows();
            if (toggle.checked) {
                const input = el(rule.input);
                if (input && !input.disabled) { input.focus(); input.select(); }
            }
            clearTimeout(retentionTimer);
            loadRetentionPreview();
        });
    });

    let previewTimer = null;
    for (const id of ['ampbb-exclusions', 'ampbb-use-defaults']) {
        el(id).addEventListener('input', () => {
            clearTimeout(previewTimer);
            previewTimer = setTimeout(previewExclusions, 400);
        });
        el(id).addEventListener('change', () => previewExclusions());
    }
}

async function boot() {
    await signIn();
    const loaded = await api('GET', '/settings');
    settings = loaded.settings;
    defaults = {
        hot: loaded.default_hot || [],
        excludes: loaded.default_excludes || [],
        retention: loaded.default_retention || {},
    };
    wire();
    renderSettings();
    await refresh();
    openStream();
    /* A slow poll behind the stream: it catches anything that changed without
     * producing a job event -- the instance being stopped from AMP's own tab,
     * for instance -- and costs one request a minute. */
    clearInterval(refreshTimer);
    refreshTimer = setInterval(() => refresh().catch(() => {}), 60000);
}

/* --- the module AMP's loader expects -------------------------------------- */

/* AMP turns a sidebar entry's display name into its URL by stripping spaces
 * and anything in brackets, then lower-casing it (UI.js, SideMenuEntryVM). So
 * "Backups (amp-bb)" becomes "/backups" -- exactly the path AMP's own Backups
 * tab already owns. Worse, the popstate handler resolves a path with
 * find(m => m.shortName == handler), which returns whichever entry was
 * registered first: ours would be unreachable by URL, and a reload would land
 * on AMP's Backups instead.
 *
 * shortName is a plain property, read when the entry is clicked rather than at
 * construction, so overriding it afterwards is enough. The label stays as it
 * reads best in the sidebar; only the path changes.
 */
function claimOurOwnURL() {
    try {
        if (typeof UI === 'undefined' || !UI.GetSideMenuItem) { return; }
        const vm = UI.GetSideMenuItem(AMPBB_TAB_ID);
        if (vm) { vm.shortName = 'ampbb'; }
    } catch (err) {
        // A tab that works but has a clashing URL beats no tab at all.
        console.error('amp-bb: could not claim a URL of its own:', err);
    }
}

this.stylesheet = 'amp-bb.css';

this.tabs = [{
    Name: 'Backups (amp-bb)',
    ShortName: 'ampbb',
    Icon: 'backup',
    File: 'tab.html',
    /* Just after AMP's own Backups entry, so the two sit together. */
    Order: 85,
}];

this.plugin = {
    PostInit: function () {
        claimOurOwnURL();
        boot().catch((err) => {
            /* Failing visibly but harmlessly is the requirement: this is
             * injected into somebody else's panel, and an exception thrown
             * into AMP's console would be our bug looking like theirs. */
            const host = document.querySelector(AMPBB_TAB);
            if (host) {
                host.appendChild(banner('error', 'amp-bb could not start',
                    err && err.message ? err.message : String(err)));
            }
            if (typeof console !== 'undefined') { console.error('amp-bb:', err); }
        });
    },
};
