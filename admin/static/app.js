// Phaethon Admin - Frontend JavaScript

// ========== Global State ==========
// Version-vector state:
//   targetVersions: latest version seen from SSE (what we should be fetching).
//   cachedVersions: version of the data currently rendered / cached.
// The cached version is only updated after a fetch completes and the response
// is still for the current target — never immediately on notification.
const targetVersions = {};
const cachedVersions = {};
// Topics whose last fetch failed; used to show a sync-error indicator.
const topicErrors = new Set();

// Shared subscription-node health cache, persisted to localStorage so results
// survive modal re-opens and server restarts. Used by both /subscriptions and
// the proxy-group node popup.
const SUB_HEALTH_STORAGE_KEY = 'phaethon-sub-health-v1';
window.subHealthCache = {};

function loadSubHealthCache() {
    try {
        const raw = localStorage.getItem(SUB_HEALTH_STORAGE_KEY);
        if (raw) Object.assign(window.subHealthCache, JSON.parse(raw));
    } catch (e) {
        // ignore corrupt storage
    }
}

function persistSubHealthCache() {
    try {
        localStorage.setItem(SUB_HEALTH_STORAGE_KEY, JSON.stringify(window.subHealthCache));
    } catch (e) {
        // ignore quota errors
    }
}

loadSubHealthCache();

// ========== Init ==========
document.addEventListener('DOMContentLoaded', () => {
    VersionNotificationService.start();
    registerDefaultVersionHandlers();
    updateUptime();

    // Initial load for connection logs if on dashboard
    if (document.getElementById('conn-logs')) {
        fetchConnections();
    }

    // Initial load for active connections if on dashboard
    if (document.getElementById('active-conns-list')) {
        fetchActiveConns();
        startActiveConnsTimer();
    }

    // Initial load for TUN status if on dashboard
    if (document.getElementById('tun-status')) {
        fetchTUNStatus();
    }

    // Tear down SSE on full page unload (browser close/refresh).
    // HTMX navigation does NOT cause full page loads, so no teardown on nav clicks.
    function teardown() {
        VersionNotificationService.stop();
    }
    window.addEventListener('beforeunload', teardown);
    window.addEventListener('pagehide', teardown);

    // HTMX SPA: after content swap, re-execute inline scripts, apply i18n, update nav
    document.body.addEventListener('htmx:afterSettle', function(event) {
        if (event.detail.target.id !== 'main-content') return;

        // Re-execute <script> tags in swapped content (browsers don't execute innerHTML scripts)
        const scripts = event.detail.target.querySelectorAll('script');
        scripts.forEach(function(oldScript) {
            const newScript = document.createElement('script');
            Array.from(oldScript.attributes).forEach(attr => {
                newScript.setAttribute(attr.name, attr.value);
            });
            newScript.textContent = oldScript.textContent;
            oldScript.parentNode.replaceChild(newScript, oldScript);
        });

        // Re-apply i18n to new content
        if (typeof i18n !== 'undefined' && i18n.applyTranslations) i18n.applyTranslations();

        // Update sidebar active state
        updateActiveNav();

        // Update page title in top bar
        updatePageTitle();
    });

   // Close any modal when clicking its overlay background.
    document.querySelectorAll('.modal').forEach(m => {
        m.addEventListener('click', (e) => {
            if (e.target === m) m.classList.add('hidden');
        });
    });

    setupModalResizers();
    setupUserMenu();
});

// ========== HTMX SPA Helpers ==========
const PAGE_TITLES = {
    '/': 'Dashboard',
    '/subscriptions': 'Subscriptions',
    '/proxies': 'Proxies',
    '/rules': 'Rules',
    '/mappings': 'Mappings',
    '/reverse': 'Reverse',
    '/config': 'Raw Config',
};

function updateActiveNav() {
    const path = window.location.pathname;
    document.querySelectorAll('.nav-menu a[href]').forEach(a => {
        const href = a.getAttribute('href');
        const isActive = href === path || (path === '/' && href === './') ||
                         (href === './' && path === '/') ||
                         (href.startsWith('./') && path === href.substring(1));
        a.classList.toggle('active', isActive);
    });
}

function updatePageTitle() {
    const path = window.location.pathname;
    const title = PAGE_TITLES[path] || 'Phaethon';
    const titleEl = document.getElementById('page-title');
    if (titleEl) titleEl.textContent = title;
}

function reloadPage() {
    if (typeof htmx !== 'undefined') {
        htmx.ajax('GET', window.location.pathname, {target: '#main-content', swap: 'innerHTML'});
    } else {
        location.reload();
    }
}

// ========== User Menu ==========
function setupUserMenu() {
    const menu = document.getElementById('user-menu');
    const nameEl = document.getElementById('user-name');
    if (!menu || !nameEl) return;

    fetch('./api/me')
        .then(r => r.ok ? r.json() : null)
        .then(data => {
            if (data && data.username) {
                nameEl.textContent = data.username;
                menu.classList.remove('hidden');
            }
        })
        .catch(() => {});

    // Close dropdown when clicking outside.
    document.addEventListener('click', (e) => {
        const dropdown = document.getElementById('user-menu-dropdown');
        if (dropdown && !menu.contains(e.target)) {
            dropdown.classList.remove('show');
        }
    });
}

function toggleUserMenu() {
    const dropdown = document.getElementById('user-menu-dropdown');
    if (dropdown) dropdown.classList.toggle('show');
}

function logout() {
    fetch('./api/logout', { method: 'POST' })
        .then(() => { window.location.href = './login'; })
        .catch(() => { window.location.href = './login'; });
}

// ========== Version Notification Service ==========
// Mirrors laodeng-lab's useNotificationSSE pattern:
// - Single global SSE connection shared by all pages.
// - onBusinessVersion(topic, handler, label) registers a per-topic listener
//   and returns an unsubscribe function.
// - Each heartbeat (from SSE or polling) updates the in-memory version vector
//   and dispatches to registered handlers only when the server version is newer
//   than the handler's local version.
// - A handler's local version only advances after it succeeds.
// - If SSE heartbeats time out, the service falls back to polling /api/versions.

function showRestartBanner() {
    if (document.getElementById('server-restart-banner')) return;
    const banner = document.createElement('div');
    banner.id = 'server-restart-banner';
    banner.className = 'server-restart-banner';
    const t = (typeof i18n !== 'undefined' && i18n.t) ? i18n.t : (k, d) => d || k;
    banner.innerHTML = `
        <span class="server-restart-text">${t('server.restartDetected', 'Server restarted. Please refresh the page.')}</span>
        <button type="button" class="server-restart-btn" onclick="location.reload()">${t('server.refreshNow', 'Refresh Now')}</button>
        <button type="button" class="server-restart-close" onclick="document.getElementById('server-restart-banner').remove()" aria-label="Close">×</button>
    `;
    document.body.insertBefore(banner, document.body.firstChild);
}

const VersionNotificationService = (function () {
    const topicVersions = {};                         // latest server version per topic
    const handlers = new Map();                       // topic -> Map<id, Subscription>
    let nextHandlerId = 0;

    let sseSource = null;
    let reconnectTimer = null;
    let pollingTimer = null;
    let healthTimer = null;
    let lastHeartbeat = 0;
    let connected = false;
    let pollingMode = false;
    let consecutiveHeartbeats = 0;
    let serverBootTime = null;

    const RECONNECT_DELAY = 5000;
    const HEARTBEAT_TIMEOUT = 15000;
    const POLLING_INTERVAL = 3000;

    function applyVersions(vector) {
        const bt = vector._bootTime;
        if (bt !== undefined) {
            if (serverBootTime !== null && serverBootTime !== bt) {
                console.log('[SSE] Server restart detected, showing refresh banner');
                showRestartBanner();
            }
            serverBootTime = bt;
        }
        for (const [topic, version] of Object.entries(vector)) {
            if (typeof version === 'number') {
                topicVersions[topic] = version;
            }
        }
    }

    async function dispatchTopic(topic) {
        const topicHandlers = handlers.get(topic);
        if (!topicHandlers || topicHandlers.size === 0) return;
        const hbVersion = topicVersions[topic];
        if (hbVersion === undefined) return;

        for (const sub of topicHandlers.values()) {
            if (sub.localVersion === hbVersion) continue;
            dispatchSub(sub, hbVersion);
        }
    }

    function dispatchSub(sub, targetVersion) {
        if (sub.runningVersion !== null) {
            // Handler is already running; coalesce to the latest pending version.
            sub.pendingVersion = targetVersion;
            return;
        }
        runSub(sub, targetVersion);
    }

    async function runSub(sub, targetVersion) {
        sub.runningVersion = targetVersion;
        try {
            await sub.handler();
            sub.localVersion = targetVersion;
        } catch (err) {
            console.error('[SSE] handler failed:', err.message);
        } finally {
            sub.runningVersion = null;
            const pending = sub.pendingVersion;
            sub.pendingVersion = null;
            if (pending !== null && pending !== sub.localVersion) {
                dispatchSub(sub, pending);
            }
        }
    }

    function dispatchAll() {
        for (const topic of handlers.keys()) {
            dispatchTopic(topic);
        }
    }

    async function fetchVersions() {
        try {
            const res = await fetch('./api/versions', { cache: 'no-store' });
            if (!res.ok) throw new Error('status ' + res.status);
            const vector = await res.json();
            applyVersions(vector);
            handleVersionVector(vector);
            dispatchAll();
        } catch {}
    }

    function startPolling() {
        if (pollingTimer) return;
        pollingMode = true;
        console.log('[SSE] Heartbeat timeout, entering polling mode');
        const tick = async () => {
            if (!pollingMode) return;
            await fetchVersions();
            pollingTimer = setTimeout(tick, POLLING_INTERVAL);
        };
        pollingTimer = setTimeout(tick, 0);
    }

    function stopPolling() {
        if (pollingTimer) {
            clearTimeout(pollingTimer);
            pollingTimer = null;
        }
        pollingMode = false;
        consecutiveHeartbeats = 0;
    }

    function stopConnection() {
        if (reconnectTimer) {
            clearTimeout(reconnectTimer);
            reconnectTimer = null;
        }
        if (sseSource) {
            sseSource.close();
            sseSource = null;
            connected = false;
        }
    }

    function startConnection() {
        if (sseSource || !window.EventSource) return;

        sseSource = new EventSource('./api/events');
        sseSource.addEventListener('heartbeat', (e) => {
            try {
                lastHeartbeat = Date.now();
                consecutiveHeartbeats++;
                if (pollingMode && consecutiveHeartbeats >= 2) {
                    console.log('[SSE] Heartbeat resumed, exiting polling mode');
                    stopPolling();
                }
                const vector = JSON.parse(e.data);
                applyVersions(vector);
                handleVersionVector(vector);
                dispatchAll();
            } catch {}
        });
        sseSource.onopen = () => {
            connected = true;
            lastHeartbeat = Date.now();
            stopPolling();
        };
        sseSource.onerror = () => {
            connected = false;
            stopConnection();
            if (!pollingMode) startPolling();
            scheduleReconnect();
        };
    }

    function scheduleReconnect() {
        if (reconnectTimer) return;
        reconnectTimer = setTimeout(() => {
            reconnectTimer = null;
            if (!sseSource) startConnection();
        }, RECONNECT_DELAY);
    }

    function checkHealth() {
        if (Date.now() - lastHeartbeat > HEARTBEAT_TIMEOUT) {
            if (!pollingMode) startPolling();
            if (sseSource) {
                stopConnection();
                startConnection();
            }
        }
    }

    function register(topic, handler, label = topic) {
        let topicHandlers = handlers.get(topic);
        if (!topicHandlers) {
            topicHandlers = new Map();
            handlers.set(topic, topicHandlers);
        }
        const id = nextHandlerId++;
        const sub = { id, handler, localVersion: -1, runningVersion: null, pendingVersion: null, label };
        topicHandlers.set(id, sub);

        const currentVersion = topicVersions[topic];
        if (currentVersion !== undefined && sub.localVersion !== currentVersion) {
            dispatchTopic(topic);
        }

        return () => {
            topicHandlers.delete(id);
            if (topicHandlers.size === 0) handlers.delete(topic);
        };
    }

    function start() {
        startConnection();
        if (!healthTimer) healthTimer = setInterval(checkHealth, HEARTBEAT_TIMEOUT);
    }

    function stop() {
        stopConnection();
        stopPolling();
        if (healthTimer) {
            clearInterval(healthTimer);
            healthTimer = null;
        }
    }

    return { start, stop, register, isPolling: () => pollingMode, isConnected: () => connected };
})();

function onBusinessVersion(topic, handler, label) {
    return VersionNotificationService.register(topic, handler, label);
}

// Register the default per-topic fetch handlers. Pages/components can add
// additional listeners via onBusinessVersion(); the service deduplicates by
// handler-local version and only advances the local version after success.
function registerDefaultVersionHandlers() {
    onBusinessVersion('reverse', () => scheduleTopicFetch('reverse'), 'reverse');
    onBusinessVersion('bindings', () => scheduleTopicFetch('bindings'), 'bindings');
    onBusinessVersion('tun', () => scheduleTopicFetch('tun'), 'tun');
    onBusinessVersion('logs', () => {
        fetchConnections(true);
        fetchActiveConns(true);
        fetchStats();
        // Forward to PiP windows if open
        if (window._pipLogsWindow && !window._pipLogsWindow.closed) {
            try { window._pipLogsWindow._fetchLogs?.(false); } catch {}
        }
        if (window._pipConnsWindow && !window._pipConnsWindow.closed) {
            try { window._pipConnsWindow._fetchPipConns?.(); } catch {}
        }
    }, 'logs');
}

function startSSEUpdates() {
    VersionNotificationService.start();
}

function stopSSE() {
    VersionNotificationService.stop();
}

function handleVersionVector(vector) {
    // Keep the legacy global targetVersions map in sync with the service's
    // internal vector so existing fetch helpers can compare target/cached.
    for (const [topic, version] of Object.entries(vector)) {
        if (topic === '_bootTime') continue;
        handleVersionUpdate(topic, version, false);
    }
}

function handleVersionUpdate(topic, version, schedule = true) {
    if (targetVersions[topic] !== version) {
        targetVersions[topic] = version;
    }
    if (schedule) {
        scheduleTopicFetch(topic);
    }
}

function updateSyncStatus() {
    const el = document.getElementById('sse-status');
    if (!el) return;
    if (topicErrors.size === 0) {
        el.classList.add('hidden');
        el.textContent = '';
        return;
    }
    const label = (typeof i18n !== 'undefined' ? i18n.t('sse.syncError') : null) || '同步失败';
    const topics = Array.from(topicErrors).join(', ');
    el.textContent = label + ' (' + topics + ')';
    el.title = label + ': ' + topics;
    el.classList.remove('hidden');
}

function scheduleTopicFetch(topic) {
    if (targetVersions[topic] === cachedVersions[topic]) {
        // Already in sync: clear any stale error indicator for this topic.
        if (topicErrors.has(topic)) {
            topicErrors.delete(topic);
            updateSyncStatus();
        }
        return;
    }

    const targetVersion = targetVersions[topic];
    return fetchForTopic(topic, targetVersion)
        .then(() => {
            topicErrors.delete(topic);
            updateSyncStatus();
            // Only advance the cached version if the target hasn't moved on
            // while we were fetching.  If it has, this response is stale and
            // will be discarded by the fetch function; the service will
            // dispatch the handler again on the next heartbeat.
            if (targetVersion === targetVersions[topic]) {
                cachedVersions[topic] = targetVersion;
            }
        })
        .catch((err) => {
            // Do NOT advance cachedVersions; wait for the next heartbeat.
            topicErrors.add(topic);
            updateSyncStatus();
            throw err;
        });
}

function fetchForTopic(topic, expectedVersion) {
    switch (topic) {
        case 'bindings':
            return typeof pollReverseBindings === 'function'
                ? pollReverseBindings(undefined, expectedVersion)
                : Promise.resolve();
        case 'reverse':
            return pollReverseList(undefined, expectedVersion);
        case 'stats':
            return fetchStats(expectedVersion);
        case 'tun':
            return fetchTUNStatus(expectedVersion);
        default:
            return Promise.resolve();
    }
}

// Dirty-check cache for updateReverseStatus — skip DOM rebuild when data unchanged.
let _lastReverseStatusHash = '';
function _hashReverseStatus(data) {
    let s = '';
    for (let i = 0; i < data.length; i++) {
        const it = data[i];
        if (!it || !it.name) continue;
        s += it.name + '|';
        s += (it.assignedPort || it['assigned-port'] || 0) + '|';
        s += (it.lastError || it['last-error'] || '') + '|';
        s += (it.enabled ? '1' : '0') + '|';
        s += (it.reverseId || it['reverse-id'] || '') + ';';
    }
    return s;
}

function updateReverseStatus(data) {
    if (!data) return;

    // New multi-config format: array of reverse status items.
    if (Array.isArray(data)) {
        // Dirty check: skip full rebuild if nothing changed.
        const hash = _hashReverseStatus(data);
        if (hash === _lastReverseStatusHash) return;
        _lastReverseStatusHash = hash;
        const statusEl = document.getElementById('reverse-status');
        if (statusEl) {
            let key;
            if (!data.length) {
                key = 'rv.statusNotConfigured';
            } else if (data.some(item => item && item.enabled)) {
                key = 'rv.statusEnabled';
            } else {
                key = 'rv.statusDisabled';
            }
            statusEl.innerHTML = `<span data-i18n="${key}">${i18n.t(key)}</span>`;
        }

        // Keep the instance-level ReverseID badge in sync with SSE/REST updates.
        const instanceBadge = document.getElementById('instance-reverse-id');
        if (instanceBadge) {
            for (let i = 0; i < data.length; i++) {
                const item = data[i];
                if (item && (item.reverseId || item['reverse-id'])) {
                    instanceBadge.textContent = item.reverseId || item['reverse-id'];
                    break;
                }
            }
        }

        const listBody = document.getElementById('rv-list-body');
        const listTable = document.getElementById('rv-list-table');
        const emptyState = document.getElementById('rv-empty-state');
        if (listBody) {
            if (!data.length) {
                listBody.innerHTML = '';
                if (listTable) listTable.style.display = 'none';
                if (emptyState) emptyState.style.display = 'block';
            } else {
                if (listTable) listTable.style.display = '';
                if (emptyState) emptyState.style.display = 'none';
                listBody.innerHTML = data.map(item => {
                    if (!item || !item.name) return '';
                    const proto = (item.registerProto || item['register-proto'] || 'socks5').toUpperCase();
                    const listenerProto = (item.listenerProto || item['listener-proto'] || 'socks5').toUpperCase();
                    const registryAddr = item.registryAddr || item['registry-addr'] || '';
                    const outboundProxy = item.outboundProxy || item['outbound-proxy'] || '';
                    const target = item.targetAddress || item['target-address'] || '';
                    const assignedPort = item.assignedPort || item['assigned-port'] || 0;
                    const lastError = item.lastError || item['last-error'] || '';
                    const reverseId = item.reverseId || item['reverse-id'] || '';
                    const seq = item.seq || 0;
                    const enabled = item.enabled === true;
                    const endpoint = assignedPort
                        ? `${listenerProto}://${(registryAddr.split(':')[0] || '')}:${assignedPort}`
                        : lastError ? (i18n.t('rv.registrationFailed') || '注册失败')
                        : `<span class="spinner"></span> <span class="waiting">${i18n.t('rv.waitingForPort') || '等待分配端口'}</span>`;
                    const targetDisplay = listenerProto === 'DIRECT'
                        ? escapeHtml(target)
                        : `<span data-i18n="rv.targetDynamic">${i18n.t('rv.targetDynamic') || '动态'}</span>`;
                    let statusClass = 'badge-secondary';
                    let statusText = i18n.t('rv.statusInactive') || '未启用';
                    if (!enabled) {
                        statusClass = 'badge-secondary';
                        statusText = i18n.t('rv.statusDisabled') || '已禁用';
                    } else if (lastError) {
                        statusClass = 'badge-danger';
                        statusText = i18n.t('rv.statusError') || '错误';
                    } else if (assignedPort) {
                        statusClass = 'badge-success';
                        statusText = i18n.t('rv.statusEnabled') || '已启用';
                    }
                    return `<tr data-rv-name="${CSS.escape(item.name)}" data-rv-reverse-id="${escapeHtml(reverseId)}" data-rv-seq="${seq}" class="${enabled ? '' : 'row-disabled'}">
                        <td class="rv-name"><strong>${escapeHtml(item.name)}</strong></td>
                        <td class="rv-seq"><code>${seq}</code></td>
                        <td class="rv-reverse-id">
                            ${reverseId ? `<code class="reverse-id-short" title="${escapeHtml(reverseId)}" style="max-width: 120px; overflow: hidden; text-overflow: ellipsis; display: inline-block; vertical-align: middle;">${escapeHtml(reverseId)}</code>` : '<span class="text-muted">-</span>'}
                        </td>
                        <td class="rv-registry">${escapeHtml(outboundProxy)}</td>
                        <td class="rv-listener-proto"><code class="type-badge type-${listenerProto.toLowerCase()}">${listenerProto}</code></td>
                        <td class="rv-endpoint-cell">
                            <code class="endpoint-code rv-endpoint">${endpoint}</code>
                            <button class="btn btn-sm btn-outline rv-copy-btn" onclick="copyEndpointRow(this)" data-i18n="rv.copyEndpoint" style="display: ${assignedPort ? 'inline-flex' : 'none'}">${i18n.t('rv.copyEndpoint') || '复制'}</button>
                            <div class="rv-last-error" style="display: ${lastError ? 'block' : 'none'}; background: var(--danger-bg, #fff0f0); color: var(--danger, #c00); border: 1px solid var(--danger, #c00); border-radius: 6px; padding: 0.5rem 0.75rem; margin-top: 0.5rem; font-size: 0.9rem;">
                                <strong>${i18n.t('rv.errorLabel') || '错误'}：</strong> <span class="rv-last-error-text">${escapeHtml(lastError)}</span>
                            </div>
                        </td>
                        <td class="rv-target">${targetDisplay}</td>
                        <td>
                            <label class="switch" title="${enabled ? (i18n.t('common.enabled') || 'Enabled') : (i18n.t('rv.statusDisabled') || 'Disabled')}">
                                <input type="checkbox" ${enabled ? 'checked' : ''} onchange="toggleReverse('${escapeHtml(item.name)}', this.checked)">
                                <span class="slider"></span>
                            </label>
                        </td>
                        <td class="rv-status"><span class="badge ${statusClass}">${statusText}</span></td>
                        <td>
                            <button class="btn btn-sm btn-outline" onclick="showEditReverse('${escapeHtml(item.name)}')" data-i18n="rv.editConfig">${i18n.t('rv.editConfig') || '编辑'}</button>
                            <button class="btn btn-sm btn-danger" onclick="deleteReverseConfig('${escapeHtml(item.name)}')" data-i18n="rv.deleteConfig">${i18n.t('rv.deleteConfig') || '删除'}</button>
                        </td>
                    </tr>`;
                }).join('');
            }
        }

        // Keep the server-rendered config cache in sync so the edit form
        // always pre-fills the current state. Add new configs, update existing
        // ones, and remove entries that no longer exist.
        if (typeof existingReverseConfigs !== 'undefined' && Array.isArray(existingReverseConfigs)) {
            const seen = new Set();
            data.forEach(item => {
                if (!item || !item.name) return;
                seen.add(item.name);
                let existing = existingReverseConfigs.find(c => c && c.name === item.name);
                if (!existing) {
                    existing = {};
                    existingReverseConfigs.push(existing);
                }
                existing.name = item.name;
                existing.enabled = item.enabled;
                existing['reverse-id'] = item.reverseId || item['reverse-id'] || existing['reverse-id'] || '';
                existing.seq = item.seq || existing.seq || 0;
                existing['assigned-port'] = item.assignedPort || item['assigned-port'] || existing['assigned-port'] || 0;
                existing['last-error'] = item.lastError || item['last-error'] || existing['last-error'] || '';
                existing['outbound-proxy'] = item.outboundProxy || item['outbound-proxy'] || existing['outbound-proxy'] || '';
                existing['listener-proto'] = item.listenerProto || item['listener-proto'] || existing['listener-proto'] || 'socks5';
                existing['registry-addr'] = item.registryAddr || item['registry-addr'] || existing['registry-addr'] || '';
                existing['target-address'] = item.targetAddress || item['target-address'] || existing['target-address'] || '';
            });
            for (let i = existingReverseConfigs.length - 1; i >= 0; i--) {
                if (!seen.has(existingReverseConfigs[i].name)) {
                    existingReverseConfigs.splice(i, 1);
                }
            }
        }
        return;
    }

    // Legacy single-object format (kept for smooth transition).
    const statusEl = document.getElementById('reverse-status');
    if (statusEl && data.enabled !== undefined) {
        const key = data.enabled ? 'rv.statusEnabled' : 'rv.statusDisabled';
        statusEl.innerHTML = `<span data-i18n="${key}">${i18n.t(key)}</span>`;
    }

    const summaryCard = document.getElementById('rv-summary-card');
    if (summaryCard && data.enabled) {
        summaryCard.classList.remove('hidden');
    }

    const endpointEl = document.getElementById('rv-endpoint');
    const copyBtn = document.getElementById('rv-copy-btn');
    if (endpointEl) {
        if (data.assignedPort) {
            const proto = (data.listenerProto || 'socks5').toUpperCase();
            const host = (data.registryAddr || '').split(':')[0] || '';
            endpointEl.textContent = `${proto}://${host}:${data.assignedPort}`;
        } else if (data.lastError) {
            endpointEl.textContent = i18n.t('rv.registrationFailed');
        } else if (data.enabled) {
            endpointEl.innerHTML = '<span class="spinner"></span> <span class="waiting">' + i18n.t('rv.waitingForPort') + '</span>';
        }
    }
    if (copyBtn) {
        copyBtn.style.display = data.assignedPort ? 'inline-flex' : 'none';
    }

    const errBox = document.getElementById('rv-last-error');
    const errText = document.getElementById('rv-last-error-text');
    if (errBox && errText) {
        if (data.lastError) {
            errText.textContent = data.lastError;
            errBox.style.display = 'block';
        } else {
            errBox.style.display = 'none';
        }
    }
}

// Poll the reverse config list.  expectedVersion lets the SSE scheduler discard
// a response that arrived after a newer version event.
async function pollReverseList(force, expectedVersion) {
    const res = await fetch('./api/reverse');
    if (!res.ok) throw new Error('status ' + res.status);
    const data = await res.json();
    if (!Array.isArray(data)) throw new Error('invalid reverse data');
    if (expectedVersion !== undefined && targetVersions.reverse !== expectedVersion) return;
    updateReverseStatus(data);
}

// NOTE: A faster string-replacement version is defined below (line ~557).
// This stub is kept only for any code that might call escapeHtml before the
// file finishes parsing, but it delegates to the fast version immediately.
function _slowEscapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}

async function fetchStats(expectedVersion) {
    const res = await fetch('./api/stats');
    if (!res.ok) throw new Error('status ' + res.status);
    const data = await res.json();
    if (expectedVersion !== undefined && targetVersions.stats !== expectedVersion) return;

    // Update dashboard counters
    const activeEl = document.getElementById('active-conn');
    const totalEl = document.getElementById('total-conn');
    if (activeEl) activeEl.textContent = data.activeConnections || 0;
    if (totalEl) totalEl.textContent = data.totalConnections || 0;

    // Update top bar
    const connBadge = document.getElementById('conn-count');
    if (connBadge) connBadge.textContent = `🔗 ${data.activeConnections || 0} active`;
}

async function fetchTUNStatus(expectedVersion) {
    const res = await fetch('./api/tun');
    if (!res.ok) throw new Error('status ' + res.status);
    const data = await res.json();
    if (expectedVersion !== undefined && targetVersions.tun !== expectedVersion) return;
    if (typeof renderTUN === 'function') renderTUN(data);
}

let connLogLastSeq = 0;

// ===== Active Connections =====
var activeConnsMap = new Map();
var activeConnsLastSeq = 0;
var activeConnsTimer = null;

function formatDuration(ms) {
    const s = Math.floor(ms / 1000);
    if (s < 60) return s + 's';
    const m = Math.floor(s / 60);
    const rs = s % 60;
    if (m < 60) return m + 'm ' + rs + 's';
    const h = Math.floor(m / 60);
    const rm = m % 60;
    return h + 'h ' + rm + 'm';
}

async function fetchActiveConns(incremental) {
    const el = document.getElementById('active-conns-list');
    if (!el) return;
    try {
        let url = './api/activeconns';
        if (incremental && activeConnsLastSeq > 0) {
            url += '?after=' + activeConnsLastSeq;
        }
        const res = await fetch(url);
        if (!res.ok) throw new Error('status ' + res.status);
        const data = await res.json();

        if (data.stale) {
            activeConnsMap.clear();
            if (data.connections) {
                data.connections.forEach(c => activeConnsMap.set(c.id, c));
            }
        } else if (data.journal) {
            data.journal.forEach(e => {
                if (e.action === 'add' && e.conn) {
                    activeConnsMap.set(e.conn.id, e.conn);
                } else if (e.action === 'remove') {
                    activeConnsMap.delete(e.id);
                }
            });
        }
        activeConnsLastSeq = data.version;
        renderActiveConns();
    } catch (err) {
        console.error('fetchActiveConns error:', err);
    }
}

function renderActiveConns() {
    const el = document.getElementById('active-conns-list');
    const countEl = document.getElementById('active-conn-count');
    if (!el) return;

    const conns = Array.from(activeConnsMap.values());
    conns.sort((a, b) => new Date(a.startTime) - new Date(b.startTime));

    if (conns.length === 0) {
        el.innerHTML = '<p class="text-muted" data-i18n="dash.noActiveConns">' + i18n.t('dash.noActiveConns') + '</p>';
    } else {
        let html = '<table class="data-table" style="font-size:0.85rem;"><thead><tr>';
        html += '<th data-i18n="dash.connProtocol">' + i18n.t('dash.connProtocol') + '</th>';
        html += '<th data-i18n="dash.connDst">' + i18n.t('dash.connDst') + '</th>';
        html += '<th data-i18n="dash.connProxy">' + i18n.t('dash.connProxy') + '</th>';
        html += '<th data-i18n="dash.connDuration">' + i18n.t('dash.connDuration') + '</th>';
        html += '</tr></thead><tbody>';
        conns.forEach(c => {
            const dur = formatDuration(Date.now() - new Date(c.startTime).getTime());
            const dst = c.dstAddr + ':' + c.dstPort;
            const proxy = c.proxy || 'DIRECT';
            html += '<tr><td>' + c.protocol + '</td><td>' + dst + '</td><td>' + proxy + '</td><td data-start="' + c.startTime + '">' + dur + '</td></tr>';
        });
        html += '</tbody></table>';
        el.innerHTML = html;
    }

    if (countEl) {
        countEl.textContent = i18n.t('dash.activeConnCount').replace('{}', conns.length);
    }
}

function startActiveConnsTimer() {
    if (activeConnsTimer) return;
    activeConnsTimer = setInterval(() => {
        document.querySelectorAll('#active-conns-list td[data-start]').forEach(td => {
            td.textContent = formatDuration(Date.now() - new Date(td.dataset.start).getTime());
        });
    }, 1000);
}

function stopActiveConnsTimer() {
    if (activeConnsTimer) {
        clearInterval(activeConnsTimer);
        activeConnsTimer = null;
    }
}

async function openConnsPopup() {
    const width = Math.min(1600, Math.floor(screen.width * 0.9));
    const height = Math.floor(screen.height * 0.85);
    const left = window.screenX + (window.outerWidth - width) / 2;
    const top = window.screenY + (window.outerHeight - height) / 2;

    if (!window.documentPictureInPicture) {
        window.open('./logs', 'PhaethonConns', `width=${width},height=${height},left=${left},top=${top},resizable=yes,scrollbars=yes`);
        return;
    }

    try {
        const pipWindow = await window.documentPictureInPicture.requestWindow({ width, height });
        pipWindow.document.head.innerHTML = '';
        const pipStyle = pipWindow.document.createElement('style');
        pipStyle.textContent = `
            * { box-sizing: border-box; }
            body { margin:0; font-family:system-ui,sans-serif; background:#1a1a2e; color:#e0e0e0; }
            .pip-header { display:flex; justify-content:space-between; align-items:center; padding:0.75rem 1rem; background:#16213e; border-bottom:1px solid #333; }
            .pip-header h1 { margin:0; font-size:1.1rem; }
            .pip-actions { display:flex; gap:0.5rem; }
            .pip-btn { background:#333; border:1px solid #555; color:#e0e0e0; padding:0.25rem 0.5rem; border-radius:4px; cursor:pointer; font-size:1rem; }
            .pip-btn:hover { background:#444; }
            .pip-content { padding:0.75rem 1rem; overflow-y:auto; max-height:calc(100vh - 100px); }
            table { width:100%; border-collapse:collapse; font-size:0.85rem; }
            th, td { padding:0.4rem 0.5rem; text-align:left; border-bottom:1px solid #333; }
            th { background:#16213e; position:sticky; top:0; }
            .pip-status { padding:0.5rem 1rem; background:#16213e; border-top:1px solid #333; display:flex; justify-content:space-between; align-items:center; }
            .pip-status label { display:flex; align-items:center; gap:0.4rem; font-size:0.85rem; }
            .pip-status input { cursor:pointer; }
        `;
        pipWindow.document.head.appendChild(pipStyle);

        pipWindow.document.body.innerHTML = `
            <div class="pip-header">
                <h1 data-i18n="connpip.title">🔗 Active Connections</h1>
                <div class="pip-actions">
                    <button class="pip-btn" id="connpip-refresh">🔄</button>
                </div>
            </div>
            <div class="pip-content" id="connpip-content">
                <p data-i18n="connpip.noConns">No active connections</p>
            </div>
            <div class="pip-status">
                <span id="connpip-count" data-i18n="connpip.connCount">0 connections</span>
                <label>
                    <input type="checkbox" id="connpip-autorefresh" checked>
                    <span data-i18n="connpip.autoRefresh">Auto-refresh</span>
                </label>
            </div>
        `;

        pipWindow.document.querySelectorAll('[data-i18n]').forEach(el => {
            const key = el.dataset.i18n;
            const text = i18n.t(key);
            if (el.children.length === 0) {
                const icon = el.textContent.match(/^[📊🔗📋🔌🔄⚡📈👥💾✅❌📤🌐⚙️←→🔍📡]+\s*/);
                el.textContent = icon ? icon[0] + text : text;
            }
        });

        const contentEl = pipWindow.document.getElementById('connpip-content');
        const countEl = pipWindow.document.getElementById('connpip-count');
        const autoRefreshEl = pipWindow.document.getElementById('connpip-autorefresh');
        let pipConnsMap = new Map();
        let pipLastSeq = 0;

        function renderPipConns() {
            const conns = Array.from(pipConnsMap.values());
            conns.sort((a, b) => new Date(a.startTime) - new Date(b.startTime));
            if (conns.length === 0) {
                contentEl.innerHTML = '<p>' + i18n.t('connpip.noConns') + '</p>';
            } else {
                let html = '<table><thead><tr>';
                html += '<th>' + i18n.t('dash.connProtocol') + '</th>';
                html += '<th>' + i18n.t('dash.connDst') + '</th>';
                html += '<th>' + i18n.t('dash.connProxy') + '</th>';
                html += '<th>' + i18n.t('dash.connDuration') + '</th>';
                html += '</tr></thead><tbody>';
                conns.forEach(c => {
                    const dur = formatDuration(Date.now() - new Date(c.startTime).getTime());
                    const dst = c.dstAddr + ':' + c.dstPort;
                    const proxy = c.proxy || 'DIRECT';
                    html += '<tr><td>' + c.protocol + '</td><td>' + dst + '</td><td>' + proxy + '</td><td data-start="' + c.startTime + '">' + dur + '</td></tr>';
                });
                html += '</tbody></table>';
                contentEl.innerHTML = html;
            }
            countEl.textContent = i18n.t('connpip.connCount').replace('{}', conns.length);
        }

        const baseUrl = window.location.origin;

        async function fetchPipConns() {
            try {
                let url = baseUrl + '/api/activeconns';
                if (pipLastSeq > 0) url += '?after=' + pipLastSeq;
                const res = await fetch(url);
                if (!res.ok) throw new Error('status ' + res.status);
                const data = await res.json();
                if (data.stale) {
                    pipConnsMap.clear();
                    if (data.connections) data.connections.forEach(c => pipConnsMap.set(c.id, c));
                } else if (data.journal) {
                    data.journal.forEach(e => {
                        if (e.action === 'add' && e.conn) pipConnsMap.set(e.conn.id, e.conn);
                        else if (e.action === 'remove') pipConnsMap.delete(e.id);
                    });
                }
                pipLastSeq = data.version;
                renderPipConns();
            } catch (err) {
                console.error('PiP conns fetch error:', err);
            }
        }

        pipWindow.document.getElementById('connpip-refresh').onclick = () => { pipLastSeq = 0; fetchPipConns(); };

        let pipTimer = setInterval(() => {
            if (!autoRefreshEl.checked) return;
            contentEl.querySelectorAll('td[data-start]').forEach(td => {
                td.textContent = formatDuration(Date.now() - new Date(td.dataset.start).getTime());
            });
        }, 1000);

        pipWindow.addEventListener('pagehide', () => { clearInterval(pipTimer); });

        pipWindow._fetchPipConns = fetchPipConns;
        window._pipConnsWindow = pipWindow;
        fetchPipConns();
    } catch (err) {
        console.error('[PiP] openConnsPopup error:', err);
    }
}

async function fetchConnections(incremental) {
    const el = document.getElementById('conn-logs');
    if (!el) return;
    try {
        let url = './api/connections';
        if (incremental && connLogLastSeq > 0) {
            url += '?after=' + connLogLastSeq;
        }
        const res = await fetch(url);
        if (!res.ok) throw new Error('status ' + res.status);
        const data = await res.json();
        if (!data.logs || data.logs.length === 0) {
            if (!incremental) el.textContent = 'No logs yet';
            return;
        }
        const lines = data.logs.map(e => {
            const date = new Date(e.time);
            const hours = String(date.getHours()).padStart(2, '0');
            const minutes = String(date.getMinutes()).padStart(2, '0');
            const seconds = String(date.getSeconds()).padStart(2, '0');
            const ms = String(date.getMilliseconds()).padStart(3, '0');
            const time = `${hours}:${minutes}:${seconds}.${ms}`;
            const icon = e.status === 'ok' ? '✓' : '✗';
            const proxy = e.proxy || 'DIRECT';
            const inbound = e.inbound || '';
            let line = `${icon} [${inbound}] ${e.protocol} ${e.dstAddr}:${e.dstPort} → ${proxy}`;
            if (e.error) line += ` (${e.error})`;
            return `[${time}] ${line}`;
        });
        if (incremental && el.textContent !== 'No logs yet' && el.textContent !== '') {
            el.textContent += '\n' + lines.join('\n');
        } else {
            el.textContent = lines.join('\n');
        }
        // Trim oldest logs to prevent unbounded memory growth
        const MAX_DASHBOARD_LOG_LINES = 500;
        const allLines = el.textContent.split('\n');
        if (allLines.length > MAX_DASHBOARD_LOG_LINES) {
            el.textContent = allLines.slice(allLines.length - MAX_DASHBOARD_LOG_LINES).join('\n');
        }
        connLogLastSeq = data.logs[data.logs.length - 1].seq;
        el.scrollTop = el.scrollHeight;
    } catch (err) {
        if (!incremental) el.textContent = 'Failed to load: ' + err.message;
    }
}

async function openLogsPopup() {
    console.log('[PiP] openLogsPopup called');
    // Try Document Picture-in-Picture API first (Chrome 116+)
    if ('documentPictureInPicture' in window) {
        console.log('[PiP] API available, requesting window...');
        try {
            const pipWindow = await window.documentPictureInPicture.requestWindow({
                width: Math.min(1600, Math.floor(screen.width * 0.9)),
                height: Math.floor(screen.height * 0.85),
            });
            console.log('[PiP] Window created successfully');

            // Add styles for the PiP window
            const pipStyle = pipWindow.document.createElement('style');
            pipStyle.textContent = `
                body {
                    font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, 'Helvetica Neue', Arial, sans-serif;
                    background: #0d1117;
                    color: #c9d1d9;
                    margin: 0;
                    padding: 0;
                    height: 100vh;
                    display: flex;
                    flex-direction: column;
                }
                .pip-header {
                    background: #161b22;
                    border-bottom: 1px solid #30363d;
                    padding: 12px 20px;
                    display: flex;
                    justify-content: space-between;
                    align-items: center;
                    flex-shrink: 0;
                }
                .pip-header h1 { font-size: 16px; margin: 0; color: #f0f6fc; }
                .pip-actions { display: flex; gap: 10px; }
                .pip-btn {
                    padding: 8px 16px;
                    border-radius: 6px;
                    border: 1px solid #30363d;
                    background: #21262d;
                    color: #e6edf3;
                    cursor: pointer;
                    font-size: 14px;
                    font-weight: 500;
                }
                .pip-btn:hover { background: #30363d; }
                .pip-btn-danger { border-color: #f85149; color: #f85149; }
                .pip-btn-danger:hover { background: #f8514920; }
                #pip-logs {
                    flex: 1;
                    overflow-y: auto;
                    overflow-x: hidden;
                    padding: 16px 20px;
                    font-family: 'SF Mono', 'Monaco', 'Inconsolata', monospace;
                    font-size: 13px;
                    line-height: 1.6;
                    white-space: pre-wrap;
                    word-break: break-all;
                    margin: 0;
                    min-height: 0;
                }
                .log-line { padding: 2px 0; }
                .log-line.ok { color: #3fb950; }
                .log-line.fail, .log-line.reject { color: #f85149; }
                .log-time { color: #8b949e; margin-right: 8px; }
                .log-icon { margin-right: 4px; }
                .pip-status {
                    background: #161b22;
                    border-top: 1px solid #30363d;
                    padding: 10px 20px;
                    font-size: 13px;
                    color: #8b949e;
                    display: flex;
                    justify-content: space-between;
                    align-items: center;
                    flex-shrink: 0;
                }
                .pip-status label {
                    display: flex;
                    align-items: center;
                    gap: 6px;
                    cursor: pointer;
                }
                .pip-status input { cursor: pointer; }
            `;
            pipWindow.document.head.appendChild(pipStyle);

            // Build the UI
            pipWindow.document.body.innerHTML = `
                <div class="pip-header">
                    <h1 data-i18n="pip.title">📋 Logs</h1>
                    <div class="pip-actions">
                        <button class="pip-btn" id="pip-refresh">🔄</button>
                        <button class="pip-btn pip-btn-danger" id="pip-clear">🗑</button>
                    </div>
                </div>
                <div id="pip-logs"></div>
                <div class="pip-status">
                    <span id="pip-count" data-i18n="pip.logCount">0 logs</span>
                    <label>
                        <input type="checkbox" id="pip-autoscroll" checked>
                        <span data-i18n="pip.autoscroll">Auto-scroll</span>
                    </label>
                </div>
            `;

            // Apply i18n
            pipWindow.document.querySelectorAll('[data-i18n]').forEach(el => {
                const key = el.dataset.i18n;
                const text = i18n.t(key);
                if (el.children.length === 0) {
                    const icon = el.textContent.match(/^[📊🔗📋🔌🔄⚡📈👥💾✅❌📤🌐⚙️←→🔍📡]+\s*/);
                    el.textContent = icon ? icon[0] + text : text;
                }
            });

            const logsEl = pipWindow.document.getElementById('pip-logs');
            const countEl = pipWindow.document.getElementById('pip-count');
            const autoScrollEl = pipWindow.document.getElementById('pip-autoscroll');
            const baseUrl = window.location.origin;
            let lastSeq = 0;
            let logCount = 0;

            function appendLog(e) {
                const date = new Date(e.time);
                const hours = String(date.getHours()).padStart(2, '0');
                const minutes = String(date.getMinutes()).padStart(2, '0');
                const seconds = String(date.getSeconds()).padStart(2, '0');
                const ms = String(date.getMilliseconds()).padStart(3, '0');
                const time = `${hours}:${minutes}:${seconds}.${ms}`;
                const icon = e.status === 'ok' ? '✓' : '✗';
                const proxy = e.proxy || 'DIRECT';
                const inbound = e.inbound || '';
                let text = `${icon} [${inbound}] ${e.protocol} ${e.dstAddr}:${e.dstPort} → ${proxy}`;
                if (e.error) text += ` (${e.error})`;

                const div = pipWindow.document.createElement('div');
                div.className = `log-line ${e.status}`;
                div.innerHTML = `<span class="log-time">[${time}]</span><span class="log-icon">${text.charAt(0)}</span>${text.substring(2)}`;
                logsEl.appendChild(div);
                logCount++;
                // Trim oldest logs to prevent unbounded DOM growth
                const MAX_PIP_LOGS = 1000;
                while (logsEl.children.length > MAX_PIP_LOGS) {
                    logsEl.removeChild(logsEl.firstChild);
                }
                countEl.textContent = i18n.t('pip.logCount').replace('{}', logCount);
                // Scroll to bottom after each log for real-time updates
                scrollToBottom();
            }

            function scrollToBottom() {
                if (autoScrollEl.checked) {
                    logsEl.scrollTop = logsEl.scrollHeight;
                }
            }

            async function fetchLogs(full) {
                try {
                    let url = baseUrl + '/api/connections';
                    if (!full && lastSeq > 0) url += '?after=' + lastSeq;
                    const res = await fetch(url);
                    if (!res.ok) throw new Error('status ' + res.status);
                    const data = await res.json();
                    if (!data.logs || data.logs.length === 0) {
                        if (full) {
                            logsEl.textContent = i18n.t('pip.noLogs');
                            logCount = 0;
                            countEl.textContent = i18n.t('pip.logCount').replace('{}', 0);
                        }
                        return;
                    }
                    if (full) {
                        logsEl.textContent = '';
                        logCount = 0;
                    }
                    data.logs.forEach(e => {
                        appendLog(e);
                        lastSeq = e.seq;
                    });
                    // Use setTimeout to ensure DOM is fully updated before scrolling
                    setTimeout(() => scrollToBottom(), 50);
                } catch (err) {
                    console.error('PiP fetch error:', err);
                    if (full) logsEl.textContent = 'Failed: ' + err.message;
                }
            }

            pipWindow.document.getElementById('pip-refresh').onclick = () => fetchLogs(true);
            pipWindow.document.getElementById('pip-clear').onclick = () => {
                logsEl.textContent = i18n.t('pip.noLogs');
                logCount = 0;
                lastSeq = 0;
                countEl.textContent = i18n.t('pip.logCount').replace('{}', 0);
            };

            // Initial load
            fetchLogs(true);

            // Register PiP log fetcher so parent SSE can trigger updates
            pipWindow._fetchLogs = fetchLogs;
            window._pipLogsWindow = pipWindow;

            // Cleanup when PiP window closes
            pipWindow.addEventListener('pagehide', () => {
                pipWindow._fetchLogs = null;
                window._pipLogsWindow = null;
            });

            return;
        } catch (err) {
            console.warn('[PiP] failed, falling back to popup:', err);
        }
    } else {
        console.log('[PiP] API not available in this browser');
    }

    // Fallback: regular popup
    console.log('[PiP] Opening regular popup');
    const width = Math.min(1600, Math.floor(screen.width * 0.9));
    const height = Math.floor(screen.height * 0.85);
    const left = (screen.width - width) / 2;
    const top = (screen.height - height) / 2;
    window.open('./logs', 'PhaethonLogs', `width=${width},height=${height},left=${left},top=${top},resizable=yes,scrollbars=yes`);
}

function updateUptime() {
    const el = document.getElementById('uptime');
    if (!el) return;
    let secs = parseInt(el.dataset?.seconds || '0');
    setInterval(() => {
        secs++;
        const d = Math.floor(secs / 86400);
        const h = Math.floor((secs % 86400) / 3600);
        const m = Math.floor((secs % 3600) / 60);
        let t = '';
        if (d > 0) t += `${d}d `;
        t += `${h}h ${m}m`;
        el.textContent = `⏱ ${t}`;
    }, 60000);
}

// ========== Modal Resizers ==========
function setupModalResizers() {
    document.querySelectorAll('.modal-content').forEach(el => {
        if (el.querySelector('.modal-resizer-br')) return;
        const r = document.createElement('div');
        r.className = 'modal-resizer modal-resizer-r';
        r.dataset.dir = 'e';
        const b = document.createElement('div');
        b.className = 'modal-resizer modal-resizer-b';
        b.dataset.dir = 's';
        const br = document.createElement('div');
        br.className = 'modal-resizer modal-resizer-br';
        br.dataset.dir = 'se';
        el.appendChild(r);
        el.appendChild(b);
        el.appendChild(br);
        [r, b, br].forEach(handle => initModalResize(handle, el));
    });
}

function initModalResize(handle, content) {
    handle.addEventListener('mousedown', (e) => {
        e.preventDefault();
        e.stopPropagation();
        // Prevent a subsequent click outside the modal content from closing the modal.
        function blockClick(ev) {
            ev.stopPropagation();
            document.removeEventListener('click', blockClick, true);
        }
        document.addEventListener('click', blockClick, true);

        const dir = handle.dataset.dir;
        const startX = e.clientX;
        const startY = e.clientY;
        const rect = content.getBoundingClientRect();
        const startW = rect.width;
        const startH = rect.height;
        const minW = parseInt(getComputedStyle(content).minWidth, 10) || 320;
        const minH = parseInt(getComputedStyle(content).minHeight, 10) || 240;

        function onMove(ev) {
            const dx = ev.clientX - startX;
            const dy = ev.clientY - startY;
            if (dir.includes('e')) {
                content.style.width = Math.max(minW, startW + dx) + 'px';
            }
            if (dir.includes('s')) {
                content.style.height = Math.max(minH, startH + dy) + 'px';
            }
        }
        function onUp() {
            window.removeEventListener('mousemove', onMove);
            window.removeEventListener('mouseup', onUp);
            document.body.style.userSelect = '';
            setTimeout(() => document.removeEventListener('click', blockClick, true), 50);
        }
        document.body.style.userSelect = 'none';
        window.addEventListener('mousemove', onMove);
        window.addEventListener('mouseup', onUp);
    });
}

// ========== Config Reload ==========
async function reloadConfig() {
    try {
        const msg = typeof i18n !== 'undefined' ? i18n.t('toast.reloading') : 'Reloading configuration...';
        showToast(msg, 'info');
        const res = await fetch('./api/config/reload', { method: 'POST' });
        const data = await res.json();
        if (res.ok) {
            const okMsg = typeof i18n !== 'undefined' ? i18n.t('toast.reloadOk') : 'Config reload triggered';
            showToast('✅ ' + okMsg, 'success');
        } else {
            const failMsg = typeof i18n !== 'undefined' ? i18n.t('toast.reloadFailed') : 'Reload failed';
            showToast('❌ ' + (data.error || failMsg), 'error');
        }
    } catch (err) {
        const netErr = typeof i18n !== 'undefined' ? i18n.t('toast.networkError') : 'Network error';
        showToast('❌ ' + netErr + ': ' + err.message, 'error');
    }
}

// ========== Toast Notifications ==========
function showToast(message, type = 'success', duration = 3500) {
    let container = document.querySelector('.toast-container');
    if (!container) {
        container = document.createElement('div');
        container.className = 'toast-container';
        document.body.appendChild(container);
    }

    const toast = document.createElement('div');
    toast.className = `toast toast-${type}`;
    toast.textContent = message;
    container.appendChild(toast);

    if (duration > 0) {
        setTimeout(() => {
            toast.style.opacity = '0';
            toast.style.transition = 'opacity 0.3s';
            setTimeout(() => toast.remove(), 300);
        }, duration);
    }
    
    return toast;
}

// ========== Confirm Modal ==========
let confirmModalCallbacks = { onConfirm: null, onCancel: null };

function openConfirmModal(message, onConfirm, onCancel) {
    const modal = document.getElementById('confirm-modal');
    const msgEl = document.getElementById('confirm-modal-message');
    const okBtn = document.getElementById('confirm-modal-ok');
    if (!modal || !msgEl || !okBtn) {
        // Fallback for safety; should never happen because layout.html defines the modal.
        if (confirm(message)) { if (onConfirm) onConfirm(); }
        else { if (onCancel) onCancel(); }
        return;
    }
    confirmModalCallbacks = { onConfirm, onCancel };
    msgEl.textContent = message;
    okBtn.onclick = () => {
        modal.classList.add('hidden');
        if (onConfirm) onConfirm();
    };
    modal.classList.remove('hidden');
}

function closeConfirmModal() {
    const modal = document.getElementById('confirm-modal');
    if (modal) modal.classList.add('hidden');
    if (confirmModalCallbacks.onCancel) confirmModalCallbacks.onCancel();
}

window.showConfirm = openConfirmModal;
window.closeConfirmModal = closeConfirmModal;

// ========== Utility ==========
// JSON helper for templates
window.json = function(obj) {
    return JSON.stringify(obj).replace(/</g, '\\u003c').replace(/>/g, '\\u003e');
};

function escapeHtml(str) {
    if (str == null) return '';
    return String(str)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#039;');
}

// ========== Unified Node Viewer / Health Check ==========
window.NodeViewer = (function() {
    let opts = null;
    let allNodes = [];
    let filteredNodes = [];
    let selected = new Set();
    let health = {};
    let testing = false;

    const els = {
        modal: () => document.getElementById('node-viewer-modal'),
        title: () => document.getElementById('node-viewer-title'),
        notice: () => document.getElementById('node-viewer-notice'),
        filter: () => document.getElementById('node-viewer-filter'),
        healthUrl: () => document.getElementById('node-viewer-health-url'),
        healthUrlWrap: () => document.getElementById('node-viewer-url-wrap'),
        concurrency: () => document.getElementById('node-viewer-concurrency'),
        testAllBtn: () => document.getElementById('node-viewer-test-all'),
        selectAllBtn: () => document.getElementById('node-viewer-select-all'),
        clearBtn: () => document.getElementById('node-viewer-clear'),
        selectAliveBtn: () => document.getElementById('node-viewer-select-alive'),
        count: () => document.getElementById('node-viewer-count'),
        progress: () => document.getElementById('node-viewer-progress'),
        tableWrap: () => document.getElementById('node-viewer-table-wrap'),
        pickerWrap: () => document.getElementById('node-viewer-picker-wrap'),
        tbody: () => document.getElementById('node-viewer-tbody'),
        headCheckbox: () => document.getElementById('node-viewer-head-checkbox'),
        selectHeader: () => document.getElementById('node-viewer-select-header'),
        availableList: () => document.getElementById('node-viewer-available-list'),
        selectedList: () => document.getElementById('node-viewer-selected-list'),
        availableCount: () => document.getElementById('node-viewer-available-count'),
        selectedCount: () => document.getElementById('node-viewer-selected-count'),
        saveBtn: () => document.getElementById('node-viewer-save'),
    };

    function t(key, fallback) {
        if (typeof i18n !== 'undefined' && i18n.t) {
            const v = i18n.t(key);
            if (v) return v;
        }
        return fallback;
    }

    function normalizeHealth(raw) {
        if (!raw) return null;
        const alive = raw.Alive !== undefined ? raw.Alive : raw.alive;
        const latencyMs = raw.LatencyMs !== undefined ? raw.LatencyMs : raw.latencyMs;
        const lastCheck = raw.LastCheck || raw.lastCheck;
        const checked = alive !== undefined && alive !== null;
        return { alive, latencyMs, lastCheck, checked };
    }

    function setHealth(name, raw) {
        health[name] = normalizeHealth(raw);
        if (opts && opts.context === 'subscription' && opts.name && window.subHealthCache) {
            if (!window.subHealthCache[opts.name]) window.subHealthCache[opts.name] = {};
            window.subHealthCache[opts.name][name] = raw;
            if (typeof persistSubHealthCache === 'function') persistSubHealthCache();
        }
    }

    function getHealth(name) {
        if (health[name]) return health[name];
        if (opts && opts.context === 'subscription' && opts.name && window.subHealthCache && window.subHealthCache[opts.name]) {
            return normalizeHealth(window.subHealthCache[opts.name][name]);
        }
        return null;
    }

    function applyFilter() {
        const raw = els.filter().value.trim();
        let re = null;
        if (raw) {
            try { re = new RegExp(raw, 'i'); } catch (e) { re = null; }
        }
        filteredNodes = allNodes.filter(n => {
            if (!re) return true;
            const text = `${n.name || ''} ${n.type || ''} ${n.server || ''} ${n.port || ''}`;
            return re.test(text);
        });
    }

    function updateControls() {
        const selectable = !!(opts && opts.selectable);
        const showUrl = !!(opts && opts.showHealthUrl);
        els.selectHeader().classList.toggle('hidden', !selectable);
        els.selectAllBtn().classList.toggle('hidden', !selectable);
        els.clearBtn().classList.toggle('hidden', !selectable);
        els.selectAliveBtn().classList.toggle('hidden', !selectable);
        els.saveBtn().classList.toggle('hidden', !selectable);
        els.healthUrlWrap().classList.toggle('hidden', !showUrl);
    }

    function formatLatency(ms) {
        if (ms === undefined || ms === null) return '-';
        if (ms === 0) return t('nodeViewer.timeout', 'timeout');
        return `${ms} ms`;
    }

    function statusHtml(h) {
        if (!h || !h.checked) return '<span class="status-dot status-inactive" title="' + t('nodeViewer.unchecked','unchecked') + '"></span> <span class="text-muted">-</span>';
        if (h.alive) return '<span class="status-dot status-active" title="alive"></span> <span class="text-small">' + formatLatency(h.latencyMs) + '</span>';
        return '<span class="status-dot status-error" title="dead"></span> <span class="text-muted">' + t('nodeViewer.dead','dead') + '</span>';
    }

    function renderTable() {
        els.tableWrap().classList.remove('hidden');
        els.pickerWrap().classList.add('hidden');
        const tbody = els.tbody();
        tbody.innerHTML = '';
        if (filteredNodes.length === 0) {
            tbody.innerHTML = `<tr><td colspan="8" class="text-muted">${t('nodeViewer.noMatch','No nodes match the filter.')}</td></tr>`;
            return;
        }
        const selectable = opts.selectable;
        const frag = document.createDocumentFragment();
        filteredNodes.forEach(node => {
            const h = getHealth(node.name);
            const tr = document.createElement('tr');
            if (selectable) {
                const td = document.createElement('td');
                const cb = document.createElement('input');
                cb.type = 'checkbox';
                cb.checked = selected.has(node.name);
                cb.dataset.name = node.name;
                cb.onchange = () => toggleNode(node.name);
                td.appendChild(cb);
                tr.appendChild(td);
            }
            const cells = [
                { html: `<strong>${escapeHtml(node.name)}</strong>` },
                { html: `<code class="type-badge type-${escapeHtml((node.type || '').toLowerCase())}">${escapeHtml(node.type || '-')}</code>` },
                { text: node.server || '' },
                { text: node.port || '-' },
                { html: statusHtml(h) },
                { text: formatLatency(h && h.latencyMs) },
                { html: `<button type="button" class="btn btn-sm btn-outline node-viewer-test-btn" data-name="${escapeHtml(node.name)}">${t('nodeViewer.test','Test')}</button>` }
            ];
            cells.forEach(c => {
                const td = document.createElement('td');
                if (c.html != null) td.innerHTML = c.html;
                else td.textContent = c.text;
                tr.appendChild(td);
            });
            frag.appendChild(tr);
        });
        tbody.appendChild(frag);
        updateHeadCheckbox();
    }

    function renderPicker() {
        els.tableWrap().classList.add('hidden');
        els.pickerWrap().classList.remove('hidden');
        const available = filteredNodes.filter(n => !selected.has(n.name));
        const picked = [...selected].map(name => allNodes.find(n => n.name === name)).filter(Boolean);
        renderPickerPane(els.availableList(), available, false);
        renderPickerPane(els.selectedList(), picked, true);
        els.availableCount().textContent = available.length;
        els.selectedCount().textContent = picked.length;
    }

    function renderPickerPane(container, nodes, isSelected) {
        container.innerHTML = '';
        if (nodes.length === 0) {
            container.innerHTML = `<div class="text-muted text-small" style="padding:0.5rem;">${t('nodeViewer.empty','No nodes')}</div>`;
            return;
        }
        nodes.forEach(node => {
            const h = getHealth(node.name);
            const div = document.createElement('div');
            div.className = 'picker-item';
            let statusDot = '<span class="status-dot status-inactive"></span>';
            if (h && h.checked) statusDot = h.alive ? '<span class="status-dot status-active"></span>' : '<span class="status-dot status-error"></span>';
            div.innerHTML = `
                <div class="picker-item-main">
                    <strong>${escapeHtml(node.name)}</strong>
                    <code class="type-badge type-${escapeHtml((node.type || '').toLowerCase())}">${escapeHtml(node.type || '-')}</code>
                    ${statusDot}
                </div>
                <div class="picker-item-meta text-muted text-small">${escapeHtml(node.server || '')}:${node.port || '-'}</div>
            `;
            const btn = document.createElement('button');
            btn.type = 'button';
            btn.className = 'btn btn-sm btn-outline';
            btn.textContent = isSelected ? '×' : '+';
            btn.onclick = (e) => { e.stopPropagation(); if (isSelected) removeSelected(node.name); else addSelected(node.name); };
            div.appendChild(btn);
            div.onclick = (e) => { if (e.target === btn || btn.contains(e.target)) return; if (isSelected) removeSelected(node.name); else addSelected(node.name); };
            container.appendChild(div);
        });
    }

    function render() {
        applyFilter();
        updateControls();
        if (opts && opts.mode === 'picker') {
            renderPicker();
        } else {
            renderTable();
        }
        const tmpl = t('nodeViewer.countInfo', '{total} nodes total');
        els.count().textContent = tmpl.replace('{total}', filteredNodes.length);
    }

    function addSelected(name) {
        if (!selected.has(name)) {
            selected.add(name);
            render();
        }
    }

    function removeSelected(name) {
        if (selected.delete(name)) render();
    }

    function toggleNode(name) {
        if (selected.has(name)) selected.delete(name); else selected.add(name);
        render();
    }

    function updateHeadCheckbox() {
        const h = els.headCheckbox();
        if (!h) return;
        const selectableFiltered = filteredNodes.filter(n => opts.selectable);
        if (selectableFiltered.length === 0) { h.checked = false; h.indeterminate = false; return; }
        const checkedCount = selectableFiltered.filter(n => selected.has(n.name)).length;
        h.checked = checkedCount === selectableFiltered.length;
        h.indeterminate = checkedCount > 0 && checkedCount < selectableFiltered.length;
    }

    async function testNode(name, btn) {
        if (!opts || !opts.healthEndpoint) return;
        if (btn) {
            btn.disabled = true;
            btn.dataset.original = btn.innerHTML;
            btn.innerHTML = '<span class="spinner"></span>' + t('nodeViewer.testing','Testing');
        }
        try {
            const url = opts.healthEndpoint(name, els.healthUrl().value || '');
            const res = await fetch(url, { method: 'POST' });
            if (!res.ok) throw new Error(await res.text());
            setHealth(name, await res.json());
        } catch (err) {
            setHealth(name, { alive: false, latencyMs: 0, lastCheck: new Date().toISOString() });
            showToast(t('nodeViewer.testError','Test failed') + ': ' + err.message, 'error');
        } finally {
            if (btn) {
                btn.disabled = false;
                btn.innerHTML = btn.dataset.original || t('nodeViewer.test','Test');
            }
            render();
        }
    }

    async function testAll() {
        if (!opts || !opts.healthEndpoint || testing || filteredNodes.length === 0) return;
        testing = true;
        els.testAllBtn().disabled = true;
        const originalHTML = els.testAllBtn().innerHTML;
        els.testAllBtn().innerHTML = '<span class="spinner"></span>' + t('nodeViewer.testing','Testing');
        const progress = els.progress();
        const concurrency = Math.max(1, Math.min(50, parseInt(els.concurrency().value, 10) || 6));
        const updateProgress = (done, total) => {
            const tmpl = t('nodeViewer.testProgress','Testing {done}/{total}');
            progress.innerHTML = '<span class="spinner"></span>' + tmpl.replace('{done}', done).replace('{total}', total);
        };
        try {
            let done = 0;
            updateProgress(0, filteredNodes.length);
            for (let i = 0; i < filteredNodes.length; i += concurrency) {
                const chunk = filteredNodes.slice(i, i + concurrency);
                await Promise.all(chunk.map(async n => {
                    try {
                        const url = opts.healthEndpoint(n.name, els.healthUrl().value || '');
                        const res = await fetch(url, { method: 'POST' });
                        if (!res.ok) throw new Error(await res.text());
                        setHealth(n.name, await res.json());
                    } catch (err) {
                        setHealth(n.name, { alive: false, latencyMs: 0, lastCheck: new Date().toISOString() });
                    }
                }));
                done += chunk.length;
                updateProgress(done, filteredNodes.length);
                render();
            }
            const alive = filteredNodes.filter(n => { const h = getHealth(n.name); return h && h.alive; }).length;
            const dead = filteredNodes.length - alive;
            const tmpl = t('nodeViewer.testSummary','{alive} alive / {dead} dead / {total} total');
            progress.innerHTML = tmpl.replace('{alive}', alive).replace('{dead}', dead).replace('{total}', filteredNodes.length);
        } finally {
            testing = false;
            els.testAllBtn().disabled = false;
            els.testAllBtn().innerHTML = originalHTML;
        }
    }

    function selectAll() {
        filteredNodes.forEach(n => selected.add(n.name));
        render();
    }

    function clearSelection() {
        selected.clear();
        render();
    }

    function selectAlive() {
        filteredNodes.forEach(n => {
            const h = getHealth(n.name);
            if (h && h.alive) selected.add(n.name);
        });
        render();
    }

    function toggleSelectAll() {
        const checked = els.headCheckbox().checked;
        filteredNodes.forEach(n => {
            if (checked) selected.add(n.name); else selected.delete(n.name);
        });
        render();
    }

    async function open(options) {
        opts = options || {};
        allNodes = [];
        filteredNodes = [];
        selected = new Set();
        health = {};
        testing = false;

        if (Array.isArray(opts.selected)) opts.selected.forEach(s => selected.add(s));
        if (opts.mode !== 'picker' && opts.mode !== 'table') opts.mode = 'table';

        const titleBase = t('nodeViewer.title', 'Nodes');
        els.title().textContent = opts.title ? opts.title : titleBase;

        if (opts.notice) {
            els.notice().textContent = opts.notice;
            els.notice().classList.remove('hidden');
        } else {
            els.notice().classList.add('hidden');
        }

        els.filter().value = '';
        els.concurrency().value = '6';
        els.progress().textContent = '';
        updateControls();

        els.modal().classList.remove('hidden');
        els.tbody().innerHTML = `<tr><td colspan="8" class="text-muted">${t('nodeViewer.loading','Loading...')}</td></tr>`;

        try {
            if (typeof opts.nodes === 'function') {
                allNodes = await opts.nodes();
            } else if (Array.isArray(opts.nodes)) {
                allNodes = opts.nodes;
            }
            allNodes.forEach(n => {
                if (n && (n.alive !== undefined || n.Alive !== undefined)) {
                    health[n.name] = normalizeHealth(n);
                }
            });
            if (opts.context === 'subscription' && opts.name && window.subHealthCache && window.subHealthCache[opts.name]) {
                Object.entries(window.subHealthCache[opts.name]).forEach(([k, v]) => {
                    if (!health[k]) health[k] = normalizeHealth(v);
                });
            }
        } catch (err) {
            els.tbody().innerHTML = `<tr><td colspan="8" class="text-muted">${escapeHtml(err.message)}</td></tr>`;
            return;
        }
        render();
    }

    function close() {
        els.modal().classList.add('hidden');
        opts = null;
    }

    async function save() {
        if (!opts || !opts.onSave) return;
        const order = opts.mode === 'picker' ? [...selected] : [...selected];
        try {
            await opts.onSave(order);
            close();
        } catch (err) {
            showToast(t('nodeViewer.saveError','Save failed') + ': ' + err.message, 'error');
        }
    }

    document.addEventListener('click', (e) => {
        const btn = e.target.closest('.node-viewer-test-btn');
        if (!btn) return;
        const name = btn.dataset.name;
        if (!name) return;
        e.stopPropagation();
        testNode(name, btn);
    });

    return {
        open, close, render, testNode, testAll,
        selectAll, clearSelection, selectAlive, toggleSelectAll, save
    };
})();

// ========== Resizable Sidebar ==========
(function initResizableSidebar() {
    const sidebar = document.querySelector('.sidebar');
    const handle = document.getElementById('sidebar-resize-handle');
    const main = document.querySelector('.main-content');
    if (!handle || !sidebar || !main) return;

    let startX, startWidth, resizing = false;

    handle.addEventListener('mousedown', (e) => {
        resizing = true;
        startX = e.clientX;
        startWidth = sidebar.offsetWidth;
        handle.classList.add('resizing');
        document.body.style.userSelect = 'none';
        e.preventDefault();
    });

    document.addEventListener('mousemove', (e) => {
        if (!resizing) return;
        const newWidth = Math.min(Math.max(startWidth + e.clientX - startX, 180), 400);
        sidebar.style.width = newWidth + 'px';
        main.style.marginLeft = newWidth + 'px';
    });

    document.addEventListener('mouseup', () => {
        if (!resizing) return;
        resizing = false;
        handle.classList.remove('resizing');
        document.body.style.userSelect = '';
    });
})();

// Keyboard shortcuts
document.addEventListener('keydown', (e) => {
    // Escape closes modals
    if (e.key === 'Escape') {
        document.querySelectorAll('.modal:not(.hidden)').forEach(m => m.classList.add('hidden'));
    }
    // Ctrl+R / Cmd+R prevents browser reload, triggers config reload instead
    if ((e.ctrlKey || e.metaKey) && e.key === 'r') {
        e.preventDefault();
        reloadConfig();
    }
});
