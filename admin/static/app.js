// Phaethon Admin - Frontend JavaScript

// ========== Button loading state helper ==========
function withButtonLoading(btn, asyncFn) {
    const originalText = btn.textContent;
    const originalDisabled = btn.disabled;
    btn.disabled = true;
    btn.textContent = i18n.t('common.processing') || 'Processing...';
    
    return asyncFn().finally(() => {
        btn.disabled = originalDisabled;
        btn.textContent = originalText;
    });
}

// ========== Fetch with timeout helper ==========
async function fetchWithTimeout(url, options = {}, timeoutMs = 10000) {
    const controller = new AbortController();
    const timeoutId = setTimeout(() => controller.abort(), timeoutMs);
    try {
        const res = await fetch(url, { ...options, signal: controller.signal });
        clearTimeout(timeoutId);
        return res;
    } catch (err) {
        clearTimeout(timeoutId);
        throw err;
    }
}

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
    // Loading indicator for HTMX requests
    const indicator = document.getElementById('loading-indicator');
    const progressBar = document.getElementById('loading-progress');
    let hideTimer = null;

    console.log('[Loading Indicator] Init:', { indicator: !!indicator, progressBar: !!progressBar });

    function showLoadingIndicator(event) {
        if (!indicator || !progressBar) return;
        if (hideTimer) {
            clearTimeout(hideTimer);
            hideTimer = null;
        }
        indicator.classList.remove('hidden');
        progressBar.style.width = '0%';
        setTimeout(() => {
            if (progressBar) progressBar.style.width = '80%';
        }, 50);
    }

    function hideLoadingIndicator(event) {
        if (!indicator || !progressBar) return;
        progressBar.style.width = '100%';
        hideTimer = setTimeout(() => {
            indicator.classList.add('hidden');
            progressBar.style.width = '0%';
            hideTimer = null;
        }, 200);
    }

    document.body.addEventListener('htmx:before-request', showLoadingIndicator);
    document.body.addEventListener('htmx:after-request', hideLoadingIndicator);

    VersionNotificationService.start();
    registerDefaultVersionHandlers();
    updateUptime();

    // Initialize bottom navigation
    renderBottomNav();

    // Initial load for TUN status if on dashboard
    if (document.getElementById('tun-status')) {
        fetchTUNStatus();
    }

    // Initial load for Mesh status if on dashboard
    if (document.getElementById('mesh-card')) {
        fetchMeshStatus();
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

        // Clean up timers from previous page
        stopActiveConnsTimer();

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

        // Update bottom nav active state
        renderBottomNav();

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
    '/tun': 'TUN',
    '/mesh': 'Mesh',
    '/logs': 'Logs',
    '/connections': 'Connections',
    '/proxies': 'Proxies',
    '/rules': 'Rules',
    '/mappings': 'Mappings',
    '/resolvers': 'Resolvers',
    '/reverse': 'Reverse',
    '/admin': 'Settings',
    '/package': 'Package',
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

function toggleBottomDrawer() {
    const drawer = document.getElementById('bottom-drawer');
    if (drawer) {
        drawer.classList.toggle('hidden');
        // Setup touch handler for swipe-to-close if not already done
        if (!drawer._touchHandlerAttached) {
            drawer._touchHandlerAttached = true;
            const header = drawer.querySelector('.bottom-drawer-header');
            if (header) {
                let startY = 0;
                let currentY = 0;
                // Only attach to header to avoid interfering with body scroll
                header.addEventListener('touchstart', (e) => {
                    startY = e.touches[0].clientY;
                }, { passive: true });
                header.addEventListener('touchmove', (e) => {
                    currentY = e.touches[0].clientY;
                    const deltaY = currentY - startY;
                    if (deltaY > 0) {
                        const content = drawer.querySelector('.bottom-drawer-content');
                        if (content) {
                            content.style.transform = `translateY(${deltaY}px)`;
                        }
                    }
                }, { passive: true });
                header.addEventListener('touchend', () => {
                    const deltaY = currentY - startY;
                    if (deltaY > 100) {
                        drawer.classList.add('hidden');
                    }
                    const content = drawer.querySelector('.bottom-drawer-content');
                    if (content) {
                        content.style.transform = '';
                    }
                    startY = 0;
                    currentY = 0;
                });
            }
        }
    }
}

// Bottom navigation customization
const DEFAULT_BOTTOM_NAV = ['./', './mesh', './logs'];
const MAX_BOTTOM_NAV_ITEMS = 4;

const NAV_ITEMS = {
    './': { icon: '📊', label: 'Dashboard', i18n: 'nav.dashboard' },
    './tun': { icon: '🌐', label: 'TUN', i18n: 'nav.tun' },
    './mesh': { icon: '🔷', label: 'Mesh', i18n: 'nav.mesh' },
    './logs': { icon: '📋', label: 'Logs', i18n: 'nav.logs' },
    './connections': { icon: '🔗', label: 'Connections', i18n: 'nav.connections' },
    './proxies': { icon: '🔌', label: 'Proxies', i18n: 'nav.proxies' },
    './rules': { icon: '📜', label: 'Rules', i18n: 'nav.rules' },
    './mappings': { icon: '🗺️', label: 'Mappings', i18n: 'nav.mappings' },
    './resolvers': { icon: '↗️', label: 'Resolvers', i18n: 'nav.resolvers' },
    './reverse': { icon: '🧙', label: 'Reverse', i18n: 'nav.reverse' },
    './package': { icon: '📦', label: 'Package', i18n: 'nav.package' }
};

function getBottomNavItems() {
    const stored = localStorage.getItem('bottomNavItems');
    if (stored) {
        try {
            return JSON.parse(stored);
        } catch (e) {
            return DEFAULT_BOTTOM_NAV;
        }
    }
    return DEFAULT_BOTTOM_NAV;
}

function saveBottomNavItems(items) {
    localStorage.setItem('bottomNavItems', JSON.stringify(items));
}

function showCustomizeBottomNav() {
    toggleBottomDrawer();
    const modal = document.getElementById('customize-bottom-nav-modal');
    if (!modal) return;
    
    const currentItems = getBottomNavItems();
    const list = document.getElementById('customize-nav-list');
    if (!list) return;
    
    let html = '';
    Object.keys(NAV_ITEMS).forEach(path => {
        const item = NAV_ITEMS[path];
        const isChecked = currentItems.includes(path);
        html += `<label style="display:flex; align-items:center; gap:0.75rem; padding:0.5rem; border-radius:6px; cursor:pointer; ${isChecked ? 'background:rgba(var(--accent-rgb, 88, 166, 255), 0.1);' : ''}">`;
        html += `<input type="checkbox" value="${path}" ${isChecked ? 'checked' : ''} style="width:18px; height:18px;">`;
        html += `<span style="font-size:1.2rem;">${item.icon}</span>`;
        html += `<span style="flex:1;" data-i18n="${item.i18n}">${item.label}</span>`;
        html += '</label>';
    });
    
    list.innerHTML = html;
    modal.classList.remove('hidden');
    
    if (typeof i18n !== 'undefined' && i18n.update) {
        i18n.update(modal);
    }
}

function closeCustomizeBottomNav() {
    const modal = document.getElementById('customize-bottom-nav-modal');
    if (modal) modal.classList.add('hidden');
}

function saveBottomNavCustomization() {
    const checkboxes = document.querySelectorAll('#customize-nav-list input[type="checkbox"]:checked');
    const items = Array.from(checkboxes).map(cb => cb.value).slice(0, MAX_BOTTOM_NAV_ITEMS);
    
    if (items.length === 0) {
        if (typeof showToast === 'function') {
            showToast(i18n.t('common.selectAtLeastOne'), 'error');
        }
        return;
    }
    
    saveBottomNavItems(items);
    renderBottomNav();
    closeCustomizeBottomNav();
    
    if (typeof showToast === 'function') {
        showToast(i18n.t('common.saved'), 'success');
    }
}

function renderBottomNav() {
    const nav = document.getElementById('bottom-nav');
    if (!nav) return;
    
    const items = getBottomNavItems();
    const currentPath = window.location.pathname;
    
    let html = '';
    items.forEach(path => {
        const item = NAV_ITEMS[path];
        if (item) {
            const isActive = currentPath === path || (path === './' && currentPath === '/');
            html += `<a href="${path}" class="bottom-nav-item ${isActive ? 'active' : ''}" hx-get="${path}" hx-target="#main-content" hx-swap="innerHTML" hx-push-url="true">`;
            html += `<span class="icon">${item.icon}</span>`;
            html += `<span class="label" data-i18n="${item.i18n}">${item.label}</span>`;
            html += '</a>';
        }
    });
    
    html += `<button class="bottom-nav-item" onclick="toggleBottomDrawer()">`;
    html += `<span class="icon">☰</span>`;
    html += `<span class="label" data-i18n="nav.more">更多</span>`;
    html += '</button>';
    
    nav.innerHTML = html;
    
    // Tell HTMX to process the newly created elements
    if (typeof htmx !== 'undefined') {
        htmx.process(nav);
    }
    
    if (typeof i18n !== 'undefined' && i18n.update) {
        i18n.update(nav);
    }
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
    `;
    document.body.insertBefore(banner, document.body.firstChild);
}

const VersionNotificationService = (function () {
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
        // Check for server restart via boot version number
        const bootVersion = vector.boot;
        if (bootVersion !== undefined) {
            if (serverBootTime !== null && serverBootTime !== bootVersion) {
                console.log('[SSE] Server restart detected, showing refresh banner');
                showRestartBanner();
            }
            serverBootTime = bootVersion;
        }
        // Update all topic versions
        for (const [topic, version] of Object.entries(vector)) {
            if (typeof version === 'number') {
                targetVersions[topic] = version;
            }
        }
    }

    async function dispatchTopic(topic) {
        const topicHandlers = handlers.get(topic);
        if (!topicHandlers || topicHandlers.size === 0) return;
        const hbVersion = targetVersions[topic];
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
            const res = await fetchWithTimeout('./api/versions', { cache: 'no-store' });
            if (!res.ok) throw new Error('status ' + res.status);
            const vector = await res.json();
            applyVersions(vector);
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
                const update = JSON.parse(e.data);
                // Apply the version updates (incremental or full vector)
                applyVersions(update);
                // Dispatch only the topics that were updated
                for (const topic of Object.keys(update)) {
                    dispatchTopic(topic);
                }
            } catch {}
        });
        sseSource.onopen = () => {
            connected = true;
            lastHeartbeat = Date.now();
            // Don't stop polling here - let the heartbeat handler do it
            // after receiving 2 consecutive heartbeats to ensure connection is stable
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

        const currentVersion = targetVersions[topic];
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
    onBusinessVersion('reverse-configs', () => scheduleTopicFetch('reverse-configs'), 'reverse-configs');
    onBusinessVersion('bindings', () => {
        // Refresh bindings list if on reverse-wizard page
        if (typeof window.loadBindings === 'function') {
            window.loadBindings();
        }
    }, 'bindings');
    onBusinessVersion('mappings', () => scheduleTopicFetch('mappings'), 'mappings');
    onBusinessVersion('tun-config', () => scheduleTopicFetch('tun-config'), 'tun-config');
    onBusinessVersion('mesh', () => fetchMeshStatus(), 'mesh');
    onBusinessVersion('proxies', () => scheduleTopicFetch('proxies'), 'proxies');
    onBusinessVersion('rules', () => scheduleTopicFetch('rules'), 'rules');
    onBusinessVersion('subscriptions', () => scheduleTopicFetch('subscriptions'), 'subscriptions');
    onBusinessVersion('p2p', () => {
        // Only fetch P2P status if on proxies page
        if (typeof window.fetchP2PStatus === 'function') {
            window.fetchP2PStatus();
        }
    }, 'p2p');
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
        case 'reverse-configs':
            return pollReverseList(undefined, expectedVersion);
        case 'bindings':
            // Call global loadBindings if available (set by reverse-wizard page)
            if (typeof window.loadBindings === 'function') {
                return window.loadBindings(expectedVersion);
            }
            return Promise.resolve();
        case 'stats':
            return fetchStats(expectedVersion);
        case 'tun-config':
            return fetchTUNStatus(expectedVersion);
        case 'proxies':
            return fetchProxies(expectedVersion);
        case 'rules':
            return fetchRules(expectedVersion);
        case 'mappings':
            return fetchMappings(expectedVersion);
        case 'subscriptions':
            return fetchSubscriptions(expectedVersion);
        case 'mesh':
            return fetchMeshStatus(expectedVersion);
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
        s += (it.enabled ? '1' : '0') + ';';
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
                    const listenerProto = (item.listenerProto || item['listener-proto'] || 'socks5').toUpperCase();
                    const registryProxy = item.registryProxy || item['registry-proxy'] || '';
                    const target = item.targetAddress || item['target-address'] || '';
                    const assignedPort = item.assignedPort || item['assigned-port'] || 0;
                    const lastError = item.lastError || item['last-error'] || '';
                    const seq = item.seq || 0;
                    const enabled = item.enabled === true;
                    const portDisplay = assignedPort
                        ? `<code>${assignedPort}</code>`
                        : lastError ? `<span class="text-muted">${i18n.t('rv.registrationFailed') || '注册失败'}</span>`
                        : `<span class="waiting">${i18n.t('rv.waitingForPort') || '等待分配端口'}</span>`;
                    const targetDisplay = listenerProto === 'DIRECT'
                        ? escapeHtml(target)
                        : `<span data-i18n="rv.targetDynamic">${i18n.t('rv.targetDynamic') || '动态'}</span>`;
                    let statusClass = 'badge-secondary';
                    let statusText = i18n.t('rv.statusInactive') || '未启用';
                    let dotClass = 'status-inactive';
                    let dotTitle = 'Waiting for port';
                    if (!enabled) {
                        statusClass = 'badge-secondary';
                        statusText = i18n.t('rv.statusDisabled') || '已禁用';
                        dotClass = 'status-inactive';
                        dotTitle = 'Disabled';
                    } else if (lastError) {
                        statusClass = 'badge-danger';
                        statusText = i18n.t('rv.statusError') || '错误';
                        dotClass = 'status-error';
                        dotTitle = lastError;
                    } else if (assignedPort) {
                        statusClass = 'badge-success';
                        statusText = i18n.t('rv.statusEnabled') || '已启用';
                        dotClass = 'status-active';
                        dotTitle = 'Connected';
                    }
                    return `<tr data-rv-name="${CSS.escape(item.name)}" data-rv-seq="${seq}" data-listener-proto="${listenerProto}" data-registry-proxy="${escapeHtml(registryProxy)}" data-assigned-port="${assignedPort}" class="${enabled ? '' : 'row-disabled'}">
                        <td class="rv-name" data-label="${escapeHtml(i18n.t('rv.configName'))}"><strong>${escapeHtml(item.name)}</strong></td>
                        <td class="rv-seq" data-label="${escapeHtml(i18n.t('rv.seq'))}"><code>${seq}</code></td>
                        <td class="rv-registry" data-label="${escapeHtml(i18n.t('rv.outboundProxy'))}">${escapeHtml(registryProxy)}</td>
                        <td class="rv-listener-proto" data-label="${escapeHtml(i18n.t('rv.summaryListenProto'))}"><code class="type-badge type-${listenerProto.toLowerCase()}">${listenerProto}</code></td>
                        <td class="rv-port-cell" data-label="${escapeHtml(i18n.t('rv.listenerPort'))}">
                            <div style="flex:1; min-width:0;">
                            ${portDisplay}
                            <div class="rv-last-error" style="display: ${lastError ? 'block' : 'none'}; background: var(--danger-bg, #fff0f0); color: var(--danger, #c00); border: 1px solid var(--danger, #c00); border-radius: 6px; padding: 0.5rem 0.75rem; margin-top: 0.5rem; font-size: 0.9rem;">
                                <strong>${i18n.t('rv.errorLabel') || '错误'}：</strong> <span class="rv-last-error-text">${escapeHtml(lastError)}</span>
                            </div>
                            </div>
                        </td>
                        <td class="rv-target" data-label="${escapeHtml(i18n.t('rv.summaryTarget'))}">${targetDisplay}</td>
                        <td data-label="${escapeHtml(i18n.t('common.enabled'))}">
                            <label class="switch" title="${enabled ? (i18n.t('common.enabled') || 'Enabled') : (i18n.t('rv.statusDisabled') || 'Disabled')}">
                                <input type="checkbox" ${enabled ? 'checked' : ''} onchange="toggleReverse('${escapeHtml(item.name)}', this.checked)">
                                <span class="slider"></span>
                            </label>
                        </td>
                        <td class="rv-status" data-label="${escapeHtml(i18n.t('rv.status'))}"><span class="status-dot ${dotClass}" title="${escapeHtml(dotTitle)}"></span><span class="badge ${statusClass}">${statusText}</span></td>
                        <td data-label="${escapeHtml(i18n.t('rv.summaryOther'))}">
                            <button class="btn btn-sm btn-outline rv-copy-endpoint-btn" onclick="copyEndpointRow(this)" title="${i18n.t('rv.copyEndpoint') || '复制访问地址'}" style="display: ${assignedPort ? 'inline-flex' : 'none'}">🔗</button>
                            <button class="btn btn-sm btn-outline" onclick="copyReverse('${escapeHtml(item.name)}')" title="${i18n.t('proxy.copy') || '复制'}">📋</button>
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
                existing.seq = item.seq || existing.seq || 0;
                existing['assigned-port'] = item.assignedPort || item['assigned-port'] || existing['assigned-port'] || 0;
                existing['last-error'] = item.lastError || item['last-error'] || existing['last-error'] || '';
                existing['registry-proxy'] = item.registryProxy || item['registry-proxy'] || existing['registry-proxy'] || '';
                existing['listener-proto'] = item.listenerProto || item['listener-proto'] || existing['listener-proto'] || 'socks5';
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
            const proxyName = data.registryProxy || data['registry-proxy'] || '';
            endpointEl.textContent = `${proto}://${proxyName}:${data.assignedPort}`;
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
    const res = await fetchWithTimeout('./api/reverse');
    if (!res.ok) throw new Error('status ' + res.status);
    const data = await res.json();
    if (!Array.isArray(data)) throw new Error('invalid reverse data');
    if (expectedVersion !== undefined && targetVersions.reverse !== expectedVersion) return;
    updateReverseStatus(data);
}

// ===== Proxies client-side rendering =====
async function fetchProxies(expectedVersion) {
    const [proxiesRes, groupsRes] = await Promise.all([
        fetchWithTimeout('./api/proxies'),
        fetchWithTimeout('./api/groups')
    ]);
    if (!proxiesRes.ok || !groupsRes.ok) throw new Error('status ' + proxiesRes.status + '/' + groupsRes.status);
    const proxiesData = await proxiesRes.json();
    const groupsData = await groupsRes.json();
    if (!Array.isArray(proxiesData)) throw new Error('invalid proxies data');
    if (!Array.isArray(groupsData)) throw new Error('invalid groups data');
    if (expectedVersion !== undefined && targetVersions.proxies !== expectedVersion) {
        return;
    }
    updateProxies(proxiesData, groupsData);
    // Refresh P2P status after updating proxy list
    if (typeof window.fetchP2PStatus === 'function') {
        window.fetchP2PStatus();
    }
}

function updateProxies(proxiesData, groupsData) {
    if (typeof proxies !== 'undefined') proxies = proxiesData;
    if (typeof groups !== 'undefined') groups = groupsData;

    const tbody = document.querySelector('#proxy-table tbody');
    if (tbody) {
        tbody.innerHTML = renderProxyRows(proxiesData);
    }

    const groupsGrid = document.querySelector('.groups-grid');
    const groupsCard = document.querySelector('#proxy-table')?.closest('.page-content')?.querySelector('.card:nth-child(2)');
    if (groupsGrid) {
        if (groupsData.length === 0) {
            groupsGrid.innerHTML = '';
            const noGroups = document.createElement('p');
            noGroups.className = 'text-muted';
            noGroups.setAttribute('data-i18n', 'proxy.noGroups');
            noGroups.textContent = (typeof i18n !== 'undefined' ? i18n.t('proxy.noGroups') : null) || 'No groups defined.';
            groupsGrid.replaceWith(noGroups);
        } else {
            groupsGrid.innerHTML = renderGroupCards(groupsData);
        }
    }

    // Update proxy count in header — touch only the dedicated count span
    // so the collapsible toggle icon added around the title is preserved.
    const countEl = document.getElementById('proxies-count');
    if (countEl) {
        countEl.textContent = `(${proxiesData.length})`;
    }
}

function renderProxyRows(proxiesData) {
    return proxiesData.map(p => {
        if (!p || !p.name) return '';
        const enabled = p.enabled === true;
        const udp = p.udp === true;
        const p2p = p.p2p === true;
        const v2 = p.v2 === true;
        const rowClass = enabled ? '' : 'row-disabled';
        const pwdCell = p.password
            ? `<span class="password-container" data-proxy="${escapeHtml(p.name)}">
                <span class="password-text" id="pwd-${escapeHtml(p.name)}">••••••••</span>
                <span class="password-actual" id="pwd-actual-${escapeHtml(p.name)}" style="display:none;">${escapeHtml(p.password)}</span>
                <button onclick="togglePassword('${escapeHtml(p.name)}')" class="btn-icon" title="Show/Hide">
                    <span class="eye-icon" id="eye-${escapeHtml(p.name)}">👁️</span>
                </button>
               </span>`
            : `<span class="text-muted">-</span>`;
        // Use current P2P status from global map to avoid flickering
        let p2pCell;
        if (!p2p) {
            p2pCell = '<span class="p2p-status disabled" title="Disabled"></span>';
        } else {
            const p2pInfo = (typeof window._p2pStatusMap !== 'undefined') ? window._p2pStatusMap[p.name] : null;
            const p2pStatus = p2pInfo ? p2pInfo.status : null;
            const tf = p2pInfo ? (p2pInfo.transport || '') : '';
            const tfBadge = tf === 'udp'
                ? '<span class="p2p-transport udp" title="UDP data leg active">UDP</span>'
                : tf === 'tcp'
                    ? '<span class="p2p-transport tcp" title="UDP leg negotiated, fell back to TCP">TCP</span>'
                    : '';
            if (!p2pStatus || p2pStatus === 'connecting') {
                p2pCell = '<span class="p2p-status connecting" title="Connecting..."></span>';
            } else if (p2pStatus === 'upToDate' || p2pStatus === 'helloed') {
                const title = tf === 'udp' ? 'Connected (UDP leg)' : tf === 'tcp' ? 'Connected (TCP fallback)' : 'Connected';
                p2pCell = `<span class="p2p-status connected" title="${title}"></span>${tfBadge}`;
            } else if (p2pStatus === 'failed') {
                const err = p2pInfo.lastError || '';
                p2pCell = `<span class="p2p-status failed" title="${err ? 'Connection failed: ' + escapeHtml(err) : 'Connection failed'}"></span>`;
            } else {
                p2pCell = '<span class="p2p-status connecting" title="Connecting..."></span>';
            }
        }
        const viaCell = p.via ? escapeHtml(p.via) : '<span class="text-muted">-</span>';
        return `<tr data-name="${escapeHtml(p.name)}" class="${rowClass}">
            <td data-label="Name"><strong>${escapeHtml(p.name)}</strong></td>
            <td data-label="Type"><code class="type-badge type-${escapeHtml((p.type || '').toLowerCase())}">${escapeHtml(p.type)}</code></td>
            <td data-label="Server">${escapeHtml(p.server || '')}</td>
            <td data-label="Port">${p.port || ''}</td>
            <td data-label="Password">${pwdCell}</td>
            <td data-label="SNI">${escapeHtml(p.sni || '')}</td>
            <td data-label="UDP" style="cursor:pointer;" onclick="toggleUDP('${escapeHtml(p.name)}', ${udp})">${udp ? '✅' : '❌'}</td>
            <td data-label="P2P" style="cursor:pointer;" onclick="toggleP2P('${escapeHtml(p.name)}', ${p2p})" id="p2p-status-${escapeHtml(p.name)}">${p2pCell}</td>
            <td data-label="V2" style="cursor:pointer;" onclick="toggleV2('${escapeHtml(p.name)}', ${v2})" id="v2-status-${escapeHtml(p.name)}">${v2 ? '✅' : '❌'}</td>
            <td data-label="Via">${viaCell}</td>
            <td data-label="Enabled">
                <label class="switch" title="${enabled ? (typeof i18n !== 'undefined' ? i18n.t('common.enabled') : 'Enabled') : (typeof i18n !== 'undefined' ? i18n.t('common.disabled') : 'Disabled')}">
                    <input type="checkbox" ${enabled ? 'checked' : ''} onchange="toggleProxy('${escapeHtml(p.name)}', this.checked)">
                    <span class="slider"></span>
                </label>
            </td>
            <td data-label="Actions">
                <button onclick="editProxy('${escapeHtml(p.name)}')" class="btn btn-sm btn-outline" data-i18n="proxy.btnEdit">${(typeof i18n !== 'undefined' ? i18n.t('proxy.btnEdit') : null) || 'Edit'}</button>
                <button onclick="testProxy('${escapeHtml(p.name)}', this)" class="btn btn-sm btn-outline" data-i18n="proxy.btnTest">${(typeof i18n !== 'undefined' ? i18n.t('proxy.btnTest') : null) || 'Test'}</button>
                <button onclick="copyProxy('${escapeHtml(p.name)}')" class="btn btn-sm btn-outline" title="Copy">📋</button>
                <button onclick="deleteProxy('${escapeHtml(p.name)}')" class="btn btn-sm btn-danger" data-i18n="proxy.btnDelete">${(typeof i18n !== 'undefined' ? i18n.t('proxy.btnDelete') : null) || 'Delete'}</button>
            </td>
        </tr>`;
    }).join('');
}

async function toggleP2P(name, currentValue) {
    const newValue = !currentValue;
    try {
        const resp = await fetch(`/api/proxies/${encodeURIComponent(name)}/p2p`, {
            method: 'PATCH',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({p2p: newValue})
        });
        if (!resp.ok) {
            const err = await resp.json().catch(() => ({}));
            alert(err.error || `Toggle P2P failed: ${resp.status}`);
            return;
        }
        if (typeof loadProxies === 'function') loadProxies();
    } catch (e) {
        alert('Toggle P2P error: ' + e.message);
    }
}

async function toggleProxy(name, enabled) {
    try {
        const resp = await fetch(`/api/proxies/${encodeURIComponent(name)}`, {
            method: 'PATCH',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({enabled: enabled})
        });
        if (!resp.ok) {
            const err = await resp.json().catch(() => ({}));
            alert(err.error || `Toggle failed: ${resp.status}`);
            return;
        }
        if (typeof loadProxies === 'function') loadProxies();
    } catch (e) {
        alert('Toggle error: ' + e.message);
    }
}

function renderGroupCards(groupsData) {
    return groupsData.map(g => {
        if (!g || !g.name) return '';
        const enabled = g.enabled === true;
        const cardClass = enabled ? '' : 'row-disabled';
        const members = g.proxies || [];
        const total = members.length; // g.proxies already contains all proxies (manual + subscription)
        const activeMember = g['active-member'] || '';
        const subscription = g.subscription || '';
        const previewTags = members.slice(0, 5).map(m => {
            const mName = typeof m === 'string' ? m : (m.name || '');
            return `<span class="tag">${escapeHtml(mName)}</span>`;
        }).join('');
        return `<div class="group-card ${cardClass}" data-group="${escapeHtml(g.name)}" onclick="toggleGroupExpand('${escapeHtml(g.name)}')">
            <h4><span class="expand-chevron">▶</span> ${escapeHtml(g.name)} <span class="type-badge">${escapeHtml(g.type || '')}</span></h4>
            <p class="text-muted group-summary">
                <span data-i18n="proxy.totalMembers">${(typeof i18n !== 'undefined' ? i18n.t('proxy.totalMembers') : null) || 'Total'}</span>: ${total}
                ${activeMember ? ` · <span data-i18n="proxy.activeMember">${(typeof i18n !== 'undefined' ? i18n.t('proxy.activeMember') : null) || 'Active'}</span>: ${escapeHtml(activeMember)}` : ''}
                ${subscription ? ` · <span data-i18n="proxy.groupSub">${(typeof i18n !== 'undefined' ? i18n.t('proxy.groupSub') : null) || 'Subscription'}</span>: ${escapeHtml(subscription)}` : ''}
            </p>
            <div class="group-preview-tags">${previewTags}</div>
            <div class="group-actions" onclick="event.stopPropagation()">
                <button onclick="testGroup('${escapeHtml(g.name)}', this)" class="btn btn-sm btn-outline" data-i18n="proxy.btnTest">${(typeof i18n !== 'undefined' ? i18n.t('proxy.btnTest') : null) || 'Test'}</button>
                <label class="switch" style="margin-right:0.25rem;" title="${enabled ? 'Enabled' : 'Disabled'}">
                    <input type="checkbox" ${enabled ? 'checked' : ''} onchange="toggleGroup('${escapeHtml(g.name)}', this.checked)">
                    <span class="slider"></span>
                </label>
                <button onclick="editGroup('${escapeHtml(g.name)}')" class="btn btn-sm btn-outline" data-i18n="proxy.btnEdit">${(typeof i18n !== 'undefined' ? i18n.t('proxy.btnEdit') : null) || 'Edit'}</button>
                <button onclick="copyGroup('${escapeHtml(g.name)}')" class="btn btn-sm btn-outline" title="Copy">📋</button>
                <button onclick="deleteGroup('${escapeHtml(g.name)}')" class="btn btn-sm btn-danger" data-i18n="proxy.btnDelete">${(typeof i18n !== 'undefined' ? i18n.t('proxy.btnDelete') : null) || 'Delete'}</button>
            </div>
        </div>`;
    }).join('');
}

// ===== Rules client-side rendering =====
async function fetchRules(expectedVersion) {
    const res = await fetchWithTimeout('./api/rules');
    if (!res.ok) throw new Error('status ' + res.status);
    const data = await res.json();
    if (!Array.isArray(data)) throw new Error('invalid rules data');
    if (expectedVersion !== undefined && targetVersions.rules !== expectedVersion) return;
    updateRules(data);
}

function updateRules(data) {
    // Update the global rulesData variable for insertRuleAt
    if (typeof rulesData !== 'undefined') {
        rulesData = data;
    }

    const tbody = document.querySelector('#rules-tbody');
    if (!tbody) return;
    tbody.innerHTML = data.map((r) => {
        const id = r.id;
        const enabled = r.enabled !== false;
        const ruleText = r.rule || '';
        const rowClass = enabled ? '' : 'row-disabled';
        return `<tr data-id="${id}" class="${rowClass}">
            <td data-label="#">${id}</td>
            <td data-label="Match"><code>${escapeHtml(ruleText)}</code></td>
            <td data-label="Target">—</td>
            <td data-label="Mapping Source">—</td>
            <td data-label="Time">—</td>
            <td data-label="Enabled">
                <label class="switch" title="${enabled ? 'Enabled' : 'Disabled'}">
                    <input type="checkbox" ${enabled ? 'checked' : ''} onchange="toggleRule(${id}, this.checked)">
                    <span class="slider"></span>
                </label>
            </td>
            <td data-label="Actions">
                <button onclick="insertRuleAt(${id}, 'above')" class="btn btn-sm btn-outline">⬆ 上方插入</button>
                <button onclick="insertRuleAt(${id}, 'below')" class="btn btn-sm btn-outline">⬇ 下方插入</button>
                <button onclick="editRule(${id})" class="btn btn-sm btn-outline">Edit</button>
                <button onclick="copyRule(${id})" class="btn btn-sm btn-outline" title="Copy">📋</button>
                <button onclick="deleteRule(${id})" class="btn btn-sm btn-danger">Delete</button>
            </td>
        </tr>`;
    }).join('');

    // Update rule count
    const cardHeader = document.querySelector('.card-header h3');
    if (cardHeader) {
        const label = (typeof i18n !== 'undefined' ? i18n.t('rule.title') : null) || 'Rules';
        cardHeader.innerHTML = `<span data-i18n="rule.title">${label}</span> (${data.length})`;
    }
}

// ===== Mappings client-side rendering =====
async function fetchMappings(expectedVersion) {
    const res = await fetchWithTimeout('./api/mappings');
    if (!res.ok) throw new Error('status ' + res.status);
    const data = await res.json();
    if (!Array.isArray(data)) throw new Error('invalid mappings data');
    if (expectedVersion !== undefined && targetVersions.mappings !== expectedVersion) return;
    updateMappings(data);
}

function updateMappings(data) {
    const tbody = document.querySelector('#mapping-table tbody');
    if (!tbody) return;
    tbody.innerHTML = data.map(m => {
        if (!m || !m.name) return '';
        const enabled = m.enabled !== false;
        const rowClass = enabled ? '' : 'row-disabled';
        const dst = m.dstHost ? `${escapeHtml(m.dstHost)}:${m.dstPort}` : (m.reverseAddress || '-');
        return `<tr data-name="${escapeHtml(m.name)}" class="${rowClass}">
            <td data-label="Name"><strong>${escapeHtml(m.name)}</strong></td>
            <td data-label="Type"><code class="type-badge type-${escapeHtml((m.type || '').toLowerCase())}">${escapeHtml(m.type || '')}</code></td>
            <td data-label="Port">${m.port || ''}</td>
            <td data-label="Destination">${dst}</td>
            <td data-label="SNI">${escapeHtml(m.sni || '-')}</td>
            <td data-label="Enabled">
                <label class="switch">
                    <input type="checkbox" ${enabled ? 'checked' : ''} onchange="toggleMapping('${escapeHtml(m.name)}', this.checked)">
                    <span class="slider"></span>
                </label>
            </td>
            <td data-label="Actions">
                <button onclick="editMapping('${escapeHtml(m.name)}')" class="btn btn-sm btn-outline">${(typeof i18n !== 'undefined' ? i18n.t('common.edit') : null) || 'Edit'}</button>
                <button onclick="copyMapping('${escapeHtml(m.name)}')" class="btn btn-sm btn-outline" title="Copy">📋</button>
                <button onclick="deleteMapping('${escapeHtml(m.name)}')" class="btn btn-sm btn-danger">${(typeof i18n !== 'undefined' ? i18n.t('common.delete') : null) || 'Delete'}</button>
            </td>
        </tr>`;
    }).join('');

    const header = document.querySelector('#mapping-table')?.closest('.card')?.querySelector('.card-header h3');
    if (header) {
        const label = (typeof i18n !== 'undefined' ? i18n.t('mapping.title') : null) || 'Mappings';
        header.innerHTML = `<span data-i18n="mapping.title">${label}</span> (${data.length})`;
    }
}

// ===== Subscriptions client-side rendering =====
async function fetchSubscriptions(expectedVersion) {
    const res = await fetchWithTimeout('./api/subscriptions');
    if (!res.ok) throw new Error('status ' + res.status);
    const data = await res.json();
    if (!Array.isArray(data)) throw new Error('invalid subscriptions data');
    if (expectedVersion !== undefined && targetVersions.subscriptions !== expectedVersion) return;
    updateSubscriptions(data);
}

function updateSubscriptions(data) {
    const tbody = document.querySelector('#sub-table tbody');
    if (!tbody) return;
    if (data.length === 0) {
        tbody.innerHTML = '';
        const table = document.querySelector('#sub-table');
        if (table) table.style.display = 'none';
        let emptyState = document.querySelector('#sub-empty-state');
        if (!emptyState) {
            emptyState = document.createElement('p');
            emptyState.id = 'sub-empty-state';
            emptyState.className = 'text-muted';
            emptyState.setAttribute('data-i18n', 'sub.noSubs');
            emptyState.textContent = (typeof i18n !== 'undefined' ? i18n.t('sub.noSubs') : null) || 'No subscriptions defined.';
            table?.parentNode?.appendChild(emptyState);
        }
        emptyState.style.display = 'block';
    } else {
        const table = document.querySelector('#sub-table');
        if (table) table.style.display = '';
        const emptyState = document.querySelector('#sub-empty-state');
        if (emptyState) emptyState.style.display = 'none';
        tbody.innerHTML = data.map(sub => {
            if (!sub || !sub.name) return '';
            const enabled = sub.enabled !== false;
            const rowClass = enabled ? '' : 'row-disabled';
            return `<tr data-name="${escapeHtml(sub.name)}" data-url="${escapeHtml(sub.url || '')}" data-interval="${sub.interval || 3600}" class="${rowClass}">
                <td data-label="Name"><strong>${escapeHtml(sub.name)}</strong></td>
                <td data-label="URL" class="text-muted text-small" style="max-width: 400px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">${escapeHtml(sub.url || '')}</td>
                <td data-label="Interval">${sub.interval || 3600}s</td>
                <td data-label="Nodes">${sub.nodeCount || 0}</td>
                <td data-label="Enabled">
                    <label class="switch" title="${enabled ? 'Enabled' : 'Disabled'}">
                        <input type="checkbox" ${enabled ? 'checked' : ''} onchange="toggleSubscription('${escapeHtml(sub.name)}', this.checked)">
                        <span class="slider"></span>
                    </label>
                </td>
                <td data-label="Actions">
                    <button onclick="refreshSub('${escapeHtml(sub.name)}', this)" class="btn btn-sm btn-outline" data-i18n="sub.refresh">🔄 ${(typeof i18n !== 'undefined' ? i18n.t('sub.refresh') : null) || 'Refresh'}</button>
                    <button onclick="editSub('${escapeHtml(sub.name)}')" class="btn btn-sm btn-outline" data-i18n="sub.btnEdit">${(typeof i18n !== 'undefined' ? i18n.t('sub.btnEdit') : null) || 'Edit'}</button>
                    <button onclick="copySub('${escapeHtml(sub.name)}')" class="btn btn-sm btn-outline" title="Copy">📋</button>
                    <button onclick="deleteSub('${escapeHtml(sub.name)}')" class="btn btn-sm btn-danger" data-i18n="sub.btnDelete">${(typeof i18n !== 'undefined' ? i18n.t('sub.btnDelete') : null) || 'Delete'}</button>
                </td>
            </tr>`;
        }).join('');
    }

    const header = document.querySelector('#sub-table')?.closest('.card')?.querySelector('.card-header h3');
    if (header) {
        const label = (typeof i18n !== 'undefined' ? i18n.t('sub.title') : null) || 'Subscriptions';
        header.innerHTML = `<span data-i18n="sub.title">${label}</span> (${data.length})`;
    }
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
    updateTUNSummary(data);
}

function updateTUNSummary(data) {
    const el = document.getElementById('tun-summary');
    if (!el) return;
    const running = data.running;
    const tRunning = typeof i18n !== 'undefined' ? i18n.t(running ? 'tun.running' : 'tun.stopped') : (running ? 'Running' : 'Stopped');
    const color = running ? 'var(--success)' : 'var(--text-muted)';
    let html = '<table class="info-table">';
    html += '<tr><td>' + (typeof i18n !== 'undefined' ? i18n.t('tun.status') : 'Status') + '</td><td><span class="status-dot" style="background:' + color + '"></span> ' + tRunning + '</td></tr>';
    if (running && data.routes) {
        html += '<tr><td>' + (typeof i18n !== 'undefined' ? i18n.t('tun.tunIP') : 'TUN IP') + '</td><td>' + (data.routes.tunIP || '-') + '</td></tr>';
        html += '<tr><td>' + (typeof i18n !== 'undefined' ? i18n.t('tun.iface') : 'Interface') + '</td><td>' + (data.routes.defaultIface || '-') + '</td></tr>';
    }
    if (data.bypassGateway) {
        html += '<tr><td>' + (typeof i18n !== 'undefined' ? i18n.t('tun.bypassGateway') : 'Bypass Gateway') + '</td><td>' + (typeof i18n !== 'undefined' ? i18n.t('common.enabled') : 'Enabled') + '</td></tr>';
    }
    html += '</table>';
    el.innerHTML = html;
}

// ===== Route tree renderer (shared by mesh CIDR prefix tree + domain suffix tree) =====
// Backend node shape: {label, full, owners[], children[]}. Owner-less nodes
// are aggregate path nodes — faded. All levels default expanded on the first
// render; afterwards the previous expansion state is restored so
// version-driven re-fetches don't collapse the tree under the user.
function renderRouteTree(host, forest, emptyI18nKey) {
    const expanded = snapshotRouteTreeExpansion(host);
    host.innerHTML = '';
    if (!forest || forest.length === 0) {
        host.innerHTML = '<p class="text-muted">' + i18n.t(emptyI18nKey) + '</p>';
        return;
    }
    const frag = document.createDocumentFragment();
    forest.forEach(node => frag.appendChild(routeTreeRow(node, 0, '', expanded)));
    host.appendChild(frag);
}

// Returns a Set of "path/full" keys for nodes whose kids are currently
// visible, or null when host has no rendered tree yet (first render —
// caller falls back to defaults).
function snapshotRouteTreeExpansion(host) {
    if (!host.querySelector('.route-tree-row')) return null;
    const set = new Set();
    const walk = (nodeEl, path) => {
        const row = nodeEl.querySelector(':scope > .route-tree-row');
        const kids = nodeEl.querySelector(':scope > .route-tree-kids');
        if (!row) return;
        const full = row.querySelector('.route-tree-label').textContent;
        const key = path ? path + '/' + full : full;
        if (kids && kids.style.display !== 'none') set.add(key);
        if (kids) Array.from(kids.children).forEach(k => walk(k, key));
    };
    Array.from(host.children).forEach(el => walk(el, ''));
    return set;
}

function routeTreeRow(node, depth, path, expanded) {
    const full = node.full || node.label;
    const key = path ? path + '/' + full : full;
    const wrap = document.createElement('div');
    wrap.className = 'route-tree-node';
    const row = document.createElement('div');
    row.className = 'route-tree-row' + ((node.owners && node.owners.length) ? '' : ' route-tree-agg');
    const hasKids = node.children && node.children.length > 0;
    const toggle = document.createElement('span');
    toggle.className = 'route-tree-toggle';
    toggle.textContent = hasKids ? '▸' : '';
    row.appendChild(toggle);
    const label = document.createElement('span');
    label.className = 'route-tree-label';
    label.textContent = full;
    label.title = full;
    row.appendChild(label);
    (node.owners || []).forEach(owner => {
        const badge = document.createElement('span');
        badge.className = 'route-tree-owner';
        badge.textContent = owner;
        row.appendChild(badge);
    });
    wrap.appendChild(row);
    if (hasKids) {
        const kids = document.createElement('div');
        kids.className = 'route-tree-kids';
        const isOpen = expanded ? expanded.has(key) : true;
        if (!isOpen) kids.style.display = 'none';
        node.children.forEach(child => kids.appendChild(routeTreeRow(child, depth + 1, key, expanded)));
        wrap.appendChild(kids);
        toggle.classList.toggle('open', isOpen);
        row.addEventListener('click', () => {
            const hidden = kids.style.display === 'none';
            kids.style.display = hidden ? '' : 'none';
            toggle.classList.toggle('open', hidden);
        });
    }
    return wrap;
}

async function fetchMeshStatus() {
    const summaryEl = document.getElementById('mesh-summary');
    const card = document.getElementById('mesh-card');
    if (!card && !summaryEl) return;
    try {
        const res = await fetch('./api/mesh');
        if (!res.ok) return;
        const data = await res.json();
        updateMeshSummary(data);
        if (!data.enabled) return;
        if (!card) return;

        // Fetch full topology
        let fullTopology = { nodes: [], edges: [] };
        try {
            const topoRes = await fetch('./api/mesh/topology');
            if (topoRes.ok) {
                fullTopology = await topoRes.json();
            }
        } catch (e) {
            console.warn('fetch topology failed:', e);
        }

        // Key metrics
        document.getElementById('mesh-nodeid').textContent = data.nodeId || '-';
        document.getElementById('mesh-subnet').textContent = data.subnet || '-';
        document.getElementById('mesh-routecount').textContent = data.routeCount || 0;

        // Build a lookup for direct peers from data.peers
        const directPeers = {};
        (data.peers || []).forEach(p => { directPeers[p.nodeId] = p; });

        // Topology peers - show all nodes from fullTopology
        const peers = (data.topology && data.topology.peers) || [];
        const peerCountEl = document.getElementById('mesh-peercount');
        if (peerCountEl) peerCountEl.textContent = peers.length;

        const tbody = document.getElementById('mesh-topology');
        if (tbody) {
            let html = '';
            // Use fullTopology nodes if available, otherwise fallback to peers
            const allNodes = (fullTopology && fullTopology.nodes && fullTopology.nodes.length > 0) 
                ? fullTopology.nodes 
                : peers.map(p => ({ nodeId: p.nodeId, subnet: p.subnet, vip: '' }));
            
            // Build edges lookup for neighbors with link quality
            const edges = (fullTopology && fullTopology.edges) || [];
            const neighborsMap = {};
            edges.forEach(edge => {
                if (!neighborsMap[edge.from]) neighborsMap[edge.from] = [];
                
                // Format neighbor info with all link qualities
                // Note: each edge is directional - edge.from measured the link quality
                const formatNeighbor = (fromId, toId, links) => {
                    if (!links || links.length === 0) {
                        return { id: toId, qualities: ['-'] };
                    }
                    // Show all links with their quality and seq
                    // Format: #localSeq#remoteSeq, srtt, loss (friendlyName)
                    const qualities = links.map(link => {
                        const srtt = (link.srtt !== undefined && link.srtt !== null) ? link.srtt.toFixed(1) : '?';
                        const loss = link.lossRate ? (link.lossRate * 100).toFixed(1) : '0';
                        const retx = link.retransRate ? (link.retransRate * 100).toFixed(4) : '0';
                        const cost = (link.cost !== undefined && link.cost !== null) ? link.cost.toFixed(1) : '?';
                        const friendlyName = link.friendlyName || '';
                        const localSeq = link.localSeq || '';
                        const remoteSeq = link.remoteSeq || '';

                        let seqStr = '';
                        if (localSeq && remoteSeq) {
                            seqStr = `#${localSeq}#${remoteSeq}`;
                        } else if (localSeq) {
                            seqStr = `#${localSeq}`;
                        }

                        const parts = [];
                        if (seqStr) parts.push(seqStr);
                        parts.push(`${srtt}ms`);
                        parts.push(`loss ${loss}%`);
                        parts.push(`retx ${retx}%`);
                        parts.push(`cost ${cost}`);
                        if (friendlyName) parts.push(`(${friendlyName})`);

                        return parts.join(', ');
                    });
                    return { id: escapeHtml(toId), qualities };
                };
                
                const neighborFrom = formatNeighbor(edge.from, edge.to, edge.links);
                neighborsMap[edge.from].push(neighborFrom);
            });
            
            // Sort nodes: local node first, then direct peers, then relay nodes, alphabetically within each group
            allNodes.sort((a, b) => {
                const aIsLocal = a.nodeId === data.nodeId;
                const bIsLocal = b.nodeId === data.nodeId;
                const aIsDirect = directPeers[a.nodeId] && directPeers[a.nodeId].direct;
                const bIsDirect = directPeers[b.nodeId] && directPeers[b.nodeId].direct;
                
                // Local node first
                if (aIsLocal && !bIsLocal) return -1;
                if (!aIsLocal && bIsLocal) return 1;
                
                // Then direct peers
                if (aIsDirect && !bIsDirect) return -1;
                if (!aIsDirect && bIsDirect) return 1;
                
                // Finally alphabetically
                return a.nodeId.localeCompare(b.nodeId);
            });
            
            allNodes.forEach(n => {
                const nodeId = n.nodeId;
                const isDirect = directPeers[nodeId] && directPeers[nodeId].direct;
                const isLocal = nodeId === data.nodeId;
                const statusClass = isLocal ? 'local' : (isDirect ? 'online' : 'relay');
                const statusText = isLocal ? i18n.t('mesh.local') : (isDirect ? i18n.t('mesh.direct') : i18n.t('mesh.relay'));
                const peerData = directPeers[nodeId];
                const lastSeen = isLocal ? '-' : (peerData && peerData.lastSeen ? timeAgo(new Date(peerData.lastSeen)) : '-');
                const adminUrl = isLocal ? '#' : ('https://' + escapeHtml(nodeId) + '.phn/');
                const btnClass = isLocal ? 'btn btn-sm btn-secondary' : 'btn btn-sm btn-outline';
                const btnText = isLocal ? i18n.t('mesh.current') : i18n.t('mesh.open');
                const btnDisabled = isLocal ? 'disabled' : '';
                const neighbors = neighborsMap[nodeId] || [];
                const neighborsStr = neighbors.length > 0 
                    ? neighbors.map((n, idx) => {
                        // Neighbor name on one line
                        let html = `<div class="neighbor-block"><div class="neighbor-name">${n.id}</div>`;
                        // Each link/quality on a separate line
                        html += '<div class="neighbor-links">';
                        html += n.qualities.map(q => `<div class="neighbor-link">${q}</div>`).join('');
                        html += '</div>';
                        // Horizontal separator (except for last neighbor)
                        if (idx < neighbors.length - 1) {
                            html += '<hr class="neighbor-separator">';
                        }
                        html += '</div>';
                        return html;
                    }).join('')
                    : '-';
                html += '<tr>';
                html += '<td data-label="' + i18n.t('mesh.node') + '">' + escapeHtml(nodeId) + '</td>';
                html += '<td data-label="' + i18n.t('mesh.subnet') + '"><code>' + escapeHtml(n.subnet || '-') + '</code></td>';
                html += '<td data-label="' + i18n.t('mesh.neighbors') + '" class="neighbors-cell">' + neighborsStr + '</td>';
                html += '<td data-label="' + i18n.t('dash.listenerStatus') + '"><span class="mesh-status-dot ' + statusClass + '"></span>' + statusText + '</td>';
                html += '<td data-label="' + i18n.t('mesh.lastSeen') + '">' + lastSeen + '</td>';
                html += '<td data-label="' + i18n.t('mesh.admin') + '"><a href="' + adminUrl + '" class="' + btnClass + '" ' + btnDisabled + ' target="_blank">' + btnText + '</a></td>';
                html += '</tr>';
            });
            tbody.innerHTML = html || '<tr><td colspan="6" class="text-muted">' + i18n.t('mesh.noNodes') + '</td></tr>';
        }

        // Draw topology visualization
        drawMeshTopology(data.nodeId, peers, directPeers, fullTopology);

        // Routes: collapsible tree straight from the backend's compressed
        // prefix tree (parent/child = prefix containment, owners listed
        // side by side).
        const routesTreeHost = document.getElementById('mesh-routes-tree');
        if (routesTreeHost) {
            renderRouteTree(routesTreeHost, data.routes || [], 'mesh.noRoutes');
        }

        // Domain suffix routes: collapsible tree straight from the backend's
        // unified domain trie (the same structure the data plane resolves
        // via) — nesting mirrors suffix containment, owners listed side by
        // side.
        const domainTreeHost = document.getElementById('mesh-domain-routes-tree');
        if (domainTreeHost) {
            renderRouteTree(domainTreeHost, data.domainRoutes || [], 'mesh.noDomainRoutes');
        }
    } catch (err) {
        console.error('fetchMeshStatus error:', err);
        const summaryEl = document.getElementById('mesh-summary');
        if (summaryEl) summaryEl.innerHTML = '<p class="text-muted">' + i18n.t('mesh.loadFailed') + '</p>';
    }
}

function updateMeshSummary(data) {
    const el = document.getElementById('mesh-summary');
    if (!el) return;
    if (!data.enabled) { el.innerHTML = '<p class="text-muted">-</p>'; return; }
    const peers = (data.topology && data.topology.peers) || [];
    let html = '<table class="info-table">';
    html += '<tr><td>' + (typeof i18n !== 'undefined' ? i18n.t('mesh.nodeId') : 'Node ID') + '</td><td><code>' + (data.nodeId || '-') + '</code></td></tr>';
    html += '<tr><td>' + (typeof i18n !== 'undefined' ? i18n.t('mesh.peers') : 'Peers') + '</td><td>' + peers.length + '</td></tr>';
    html += '<tr><td>' + (typeof i18n !== 'undefined' ? i18n.t('mesh.routeCount') : 'Routes') + '</td><td>' + (data.routeCount || 0) + '</td></tr>';
    html += renderDropStatsRow(data.channelDropStats);
    html += '</table>';
    el.innerHTML = html;
}

// Renders a channel-drop-stats row for the mesh summary. Non-zero totals are
// highlighted so queue saturation is visible without opening diagnostics.
function renderDropStatsRow(stats) {
    if (!stats) return '';
    let peerDrops = 0;
    const peerMap = stats.peers || {};
    for (const k in peerMap) {
        peerDrops += (peerMap[k].DataDrops || 0) + (peerMap[k].ControlChDrops || 0);
    }
    const total = peerDrops + (stats.meshInboundDrops || 0) +
        (stats.tunWriteDrops || 0) + (stats.nicTxDrops || 0);
    const fmt = (n) => '<span' + (n > 0 ? ' style="color:var(--warn-color,#e6a23c);font-weight:600"' : '') + '>' + n + '</span>';
    const detail = 'peer:' + fmt(peerDrops) +
        ' in:' + fmt(stats.meshInboundDrops || 0) +
        ' tun:' + fmt(stats.tunWriteDrops || 0) +
        ' nic:' + fmt(stats.nicTxDrops || 0);
    const label = (typeof i18n !== 'undefined' ? i18n.t('mesh.dropStats') : 'Drop stats');
    return '<tr><td>' + label + '</td><td>' + (total === 0 ? '0' : detail) + '</td></tr>';
}

// Global state for topology drag & Drop
var _topologyState = {
    positions: {},
    dragging: null,
    dragOffset: { x: 0, y: 0 },
    canvas: null,
    width: 0,
    height: 0,
    lineOffsets: {},  // Custom curve offsets per line: groupKey -> offset
    draggingLine: null,  // Currently dragging line: {groupKey, startX, startY, startOffset}
    dragStartPos: null,
    // Viewport: world -> screen is screen = world * scale + (tx, ty).
    view: { scale: 1, tx: 0, ty: 0 },
    panning: null,        // {startX, startY, tx, ty, moved}
    autoFit: true,        // fit all nodes on first draw / after relayout
    showMetrics: true,    // draw per-link srtt/loss labels
    focusNode: null       // node highlighted by the locate controls
};

// layoutHopRings places this node at the centre and every other node on an
// elliptical ring by hop distance (BFS). Direct peers land on the first ring.
// The circular order inside a ring follows the neighbours' angular position;
// a bounded search then picks ring rotations that minimise edge crossings
// while keeping a minimum node spacing. Rings use the full canvas width
// (ellipse, not circle) so nodes are not bunched vertically.
function layoutHopRings(allNodes, edges, width, height, centerX, centerY) {
    const positions = {};
    if (!allNodes.length) return positions;

    const local = allNodes.find(n => n.isLocal) || allNodes[0];
    const adj = {};
    allNodes.forEach(n => { adj[n.id] = new Set(); });
    (edges || []).forEach(e => {
        if (adj[e.from] && adj[e.to]) {
            adj[e.from].add(e.to);
            adj[e.to].add(e.from);
        }
    });

    // BFS hop levels from this node.
    const level = new Map([[local.id, 0]]);
    const queue = [local.id];
    while (queue.length) {
        const cur = queue.shift();
        adj[cur].forEach(nb => {
            if (!level.has(nb)) {
                level.set(nb, level.get(cur) + 1);
                queue.push(nb);
            }
        });
    }
    let maxLevel = 0;
    level.forEach(v => { if (v > maxLevel) maxLevel = v; });
    // Nodes not reachable from this node (split topology): outermost ring.
    allNodes.forEach(n => { if (!level.has(n.id)) level.set(n.id, maxLevel + 1); });
    level.forEach(v => { if (v > maxLevel) maxLevel = v; });

    const compact = width < 600;
    const minRx = compact ? 92 : 170;
    const maxRx = Math.max(minRx, width * 0.42);
    const minRy = compact ? 70 : 94;
    const maxRy = Math.max(minRy, height * 0.38);

    const rings = {};
    allNodes.forEach(n => {
        const lv = level.get(n.id);
        (rings[lv] = rings[lv] || []).push(n.id);
    });
    const activeLevels = Object.keys(rings).map(Number).filter(lv => lv > 0).sort((a, b) => a - b);
    const ringCount = activeLevels.length;

    // Circular order inside each ring: sort by the circular mean of the
    // neighbours already placed on inner rings, so neighbour subtrees stay
    // adjacent. This order is fixed while rotations are searched below.
    const ringDefs = [];
    const nominal = { [local.id]: 0 };
    activeLevels.forEach((lv, ringIndex) => {
        const ids = rings[lv].slice();
        const desired = {};
        ids.forEach(id => {
            let sx = 0, sy = 0, n = 0;
            adj[id].forEach(nb => {
                const a = nominal[nb];
                if (a !== undefined) {
                    sx += Math.cos(a);
                    sy += Math.sin(a);
                    n++;
                }
            });
            if (n > 0) desired[id] = Math.atan2(sy, sx);
        });
        ids.sort((a, b) => {
            const da = desired[a], db = desired[b];
            if (da !== undefined && db !== undefined) return da - db;
            if (da !== undefined) return -1;
            if (db !== undefined) return 1;
            return a < b ? -1 : (a > b ? 1 : 0);
        });
        const step = (2 * Math.PI) / ids.length;
        ids.forEach((id, i) => { nominal[id] = -Math.PI / 2 + i * step; });
        const t = ringCount <= 1 ? 0.65 : ringIndex / (ringCount - 1);
        ringDefs.push({
            ids,
            step,
            rx: minRx + (maxRx - minRx) * t,
            ry: minRy + (maxRy - minRy) * t
        });
    });

    positions[local.id] = { x: centerX, y: centerY };
    if (!ringDefs.length) return positions;

    // Unique undirected edges for counting crossings between straight
    // approximations of the link curves.
    const edgePairs = [];
    const seenEdges = new Set();
    (edges || []).forEach(e => {
        if (e.from === e.to || !level.has(e.from) || !level.has(e.to)) return;
        const key = [e.from, e.to].sort().join('|');
        if (seenEdges.has(key)) return;
        seenEdges.add(key);
        edgePairs.push([e.from, e.to]);
    });
    const ccw = (a, b, c) => (c.y - a.y) * (b.x - a.x) - (b.y - a.y) * (c.x - a.x);
    const segmentsCross = (a, b, c, d) =>
        ccw(a, c, d) * ccw(b, c, d) < 0 && ccw(a, b, c) * ccw(a, b, d) < 0;

    const minGap = compact ? 78 : 96;
    const evaluate = (starts) => {
        const p = { [local.id]: { x: centerX, y: centerY } };
        ringDefs.forEach((ring, ri) => {
            ring.ids.forEach((id, i) => {
                const a = starts[ri] + i * ring.step;
                p[id] = { x: centerX + ring.rx * Math.cos(a), y: centerY + ring.ry * Math.sin(a) };
            });
        });

        let minDist = Infinity;
        let spacing = 0;
        const ids = Object.keys(p);
        for (let i = 0; i < ids.length; i++) {
            for (let j = i + 1; j < ids.length; j++) {
                const a = p[ids[i]];
                const b = p[ids[j]];
                const d = Math.hypot(a.x - b.x, a.y - b.y);
                if (d < minDist) minDist = d;
                if (d < minGap) spacing += (minGap - d) * (minGap - d);
            }
        }

        let crossings = 0;
        for (let i = 0; i < edgePairs.length; i++) {
            for (let j = i + 1; j < edgePairs.length; j++) {
                const e1 = edgePairs[i];
                const e2 = edgePairs[j];
                if (e1[0] === e2[0] || e1[0] === e2[1] || e1[1] === e2[0] || e1[1] === e2[1]) continue;
                if (segmentsCross(p[e1[0]], p[e1[1]], p[e2[0]], p[e2[1]])) crossings++;
            }
        }

        return {
            positions: p,
            score: crossings * 200 - minDist * 0.4 + spacing * 2
        };
    };

    // Bounded rotation search: exhaustive for normal topologies, coordinate
    // descent for very deep ones (many rings).
    let steps = 24;
    while (Math.pow(steps, ringDefs.length) > 150000 && steps > 6) {
        steps = Math.floor(steps / 2);
    }
    const candidates = [];
    for (let k = 0; k < steps; k++) candidates.push(-Math.PI + k * (2 * Math.PI / steps));

    let best = null;
    if (Math.pow(steps, ringDefs.length) <= 150000) {
        const starts = new Array(ringDefs.length).fill(-Math.PI / 2);
        const walk = (idx) => {
            if (idx === ringDefs.length) {
                const result = evaluate(starts);
                if (!best || result.score < best.score) best = result;
                return;
            }
            for (let k = 0; k < candidates.length; k++) {
                starts[idx] = candidates[k];
                walk(idx + 1);
            }
        };
        walk(0);
    } else {
        const starts = ringDefs.map(() => -Math.PI / 2);
        for (let pass = 0; pass < 2; pass++) {
            ringDefs.forEach((_, ri) => {
                let bestAngle = starts[ri];
                let bestScore = Infinity;
                candidates.forEach(angle => {
                    const trial = starts.slice();
                    trial[ri] = angle;
                    const result = evaluate(trial);
                    if (result.score < bestScore) {
                        bestScore = result.score;
                        bestAngle = angle;
                    }
                });
                starts[ri] = bestAngle;
            });
        }
        best = evaluate(starts);
    }

    Object.assign(positions, best.positions);
    return positions;
}

// fitTopologyView selects a scale/offset so every node (plus label margin)
// is visible inside the canvas.
function fitTopologyView(width, height, positions) {
    const ids = Object.keys(positions || {});
    if (!ids.length || width <= 0 || height <= 0) {
        _topologyState.view = { scale: 1, tx: 0, ty: 0 };
        return;
    }
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    ids.forEach(id => {
        const p = positions[id];
        minX = Math.min(minX, p.x);
        minY = Math.min(minY, p.y);
        maxX = Math.max(maxX, p.x);
        maxY = Math.max(maxY, p.y);
    });
    const pad = 56; // room for node labels / edge pills
    const bw = Math.max(1, (maxX - minX) + pad * 2);
    const bh = Math.max(1, (maxY - minY) + pad * 2);
    let scale = Math.min(width / bw, height / bh);
    scale = Math.max(0.4, Math.min(scale, 1.6));
    const cx = (minX + maxX) / 2;
    const cy = (minY + maxY) / 2;
    _topologyState.view = {
        scale: scale,
        tx: width / 2 - cx * scale,
        ty: height / 2 - cy * scale
    };
}

function redrawMeshTopology() {
    if (!_topologyState.localNodeId) return;
    drawMeshTopology(
        _topologyState.localNodeId,
        _topologyState.peers || [],
        _topologyState.directPeers || {},
        _topologyState.fullTopology
    );
}

function meshTopoZoomAt(factor, cx, cy) {
    const st = _topologyState;
    if (cx === undefined) cx = st.width / 2;
    if (cy === undefined) cy = st.height / 2;
    const v = st.view;
    const next = Math.max(0.35, Math.min(3, v.scale * factor));
    if (next === v.scale) return;
    // Keep the world point under the cursor fixed on screen.
    const wx = (cx - v.tx) / v.scale;
    const wy = (cy - v.ty) / v.scale;
    v.scale = next;
    v.tx = cx - wx * next;
    v.ty = cy - wy * next;
    redrawMeshTopology();
}

function meshTopoZoomBy(factor) {
    meshTopoZoomAt(factor);
}

function meshTopoFit() {
    _topologyState.autoFit = false;
    fitTopologyView(_topologyState.width, _topologyState.height, _topologyState.positions);
    redrawMeshTopology();
}

function meshTopoCenterOn(id) {
    const p = _topologyState.positions[id];
    if (!p) return;
    const v = _topologyState.view;
    if (v.scale < 1) v.scale = 1;
    v.tx = _topologyState.width / 2 - p.x * v.scale;
    v.ty = _topologyState.height / 2 - p.y * v.scale;
    _topologyState.focusNode = id;
    redrawMeshTopology();
}

function meshTopoLocateSelf() {
    meshTopoCenterOn(_topologyState.localNodeId);
}

function meshTopoLocateNode(id) {
    if (id) meshTopoCenterOn(id);
}

function meshTopoRelayout() {
    _topologyState.positions = {};
    _topologyState.lineOffsets = {};
    _topologyState.focusNode = null;
    _topologyState.autoFit = true;
    redrawMeshTopology();
}

function meshTopoToggleMetrics(on) {
    _topologyState.showMetrics = !!on;
    redrawMeshTopology();
}

function syncTopoNodeSelect(allNodes) {
    const sel = document.getElementById('mesh-topo-node-select');
    if (!sel) return;
    const ids = allNodes.map(n => n.id).sort();
    const sig = ids.join(',');
    if (sel.dataset.sig === sig) return;
    const prev = sel.value;
    const placeholder = typeof i18n !== 'undefined' ? i18n.t('mesh.locateNode') : '定位节点…';
    sel.innerHTML = '';
    const blank = document.createElement('option');
    blank.value = '';
    blank.textContent = placeholder;
    sel.appendChild(blank);
    ids.forEach(id => {
        const opt = document.createElement('option');
        opt.value = id;
        opt.textContent = id;
        sel.appendChild(opt);
    });
    sel.dataset.sig = sig;
    if (ids.indexOf(prev) >= 0) sel.value = prev;
}

// topologyLinkGroups returns every drawn link curve with its geometry. It is
// shared by hit-testing (Shift+drag curve adjustment, hover tooltip) so both
// use exactly the curves the canvas shows.
function topologyLinkGroups(positions) {
    const fullTopology = _topologyState.fullTopology;
    if (!fullTopology || !fullTopology.edges) return [];

    const nodePairEdges = {};
    fullTopology.edges.forEach(edge => {
        if (!edge.links || edge.links.length === 0) return;
        const pairKey = [edge.from, edge.to].sort().join('|');
        (nodePairEdges[pairKey] = nodePairEdges[pairKey] || []).push(...edge.links.map(link => ({
            from: edge.from, to: edge.to, link
        })));
    });

    const linkGroups = [];
    Object.values(nodePairEdges).forEach(edgeList => {
        const used = new Set();
        edgeList.forEach((edge, idx) => {
            if (used.has(idx)) return;
            const group = { edges: [edge] };
            used.add(idx);
            const localSeq = edge.link.localSeq;
            const remoteSeq = edge.link.remoteSeq;
            edgeList.forEach((other, otherIdx) => {
                if (used.has(otherIdx)) return;
                if (other.link.localSeq === remoteSeq && other.link.remoteSeq === localSeq) {
                    group.edges.push(other);
                    used.add(otherIdx);
                }
            });
            linkGroups.push(group);
        });
    });

    const nodePairGroups = {};
    linkGroups.forEach((group, groupIdx) => {
        if (group.edges.length === 0) return;
        const firstEdge = group.edges[0];
        const pairKey = [firstEdge.from, firstEdge.to].sort().join('|');
        (nodePairGroups[pairKey] = nodePairGroups[pairKey] || []).push({ ...group, index: groupIdx });
    });

    const result = [];
    Object.values(nodePairGroups).forEach(groups => {
        if (!groups.length) return;
        const firstEdge = groups[0].edges[0];
        const from = positions[firstEdge.from];
        const to = positions[firstEdge.to];
        if (!from || !to) return;
        const dx = to.x - from.x;
        const dy = to.y - from.y;
        const len = Math.sqrt(dx * dx + dy * dy);
        if (len === 0) return;
        const nx = -dy / len;
        const ny = dx / len;
        groups.forEach((group, groupIndex) => {
            const defaultOffset = (groupIndex - (groups.length - 1) / 2) * (groups.length > 1 ? 60 : 30);
            const groupKey = `${group.edges[0].from}-${group.edges[0].to}-${group.index}`;
            const curveOffset = _topologyState.lineOffsets[groupKey] !== undefined
                ? _topologyState.lineOffsets[groupKey]
                : defaultOffset;
            const midX = (from.x + to.x) / 2;
            const midY = (from.y + to.y) / 2;
            result.push({
                groupKey,
                from: firstEdge.from,
                to: firstEdge.to,
                fromPos: from,
                toPos: to,
                nx, ny,
                startOffset: curveOffset,
                cpX: midX + nx * curveOffset,
                cpY: midY + ny * curveOffset,
                edges: group.edges
            });
        });
    });
    return result;
}

function findTopologyLinkAt(x, y, threshold) {
    let best = null;
    let bestDist = threshold;
    topologyLinkGroups(_topologyState.positions).forEach(g => {
        for (let t = 0; t <= 1; t += 0.05) {
            const px = (1 - t) * (1 - t) * g.fromPos.x + 2 * (1 - t) * t * g.cpX + t * t * g.toPos.x;
            const py = (1 - t) * (1 - t) * g.fromPos.y + 2 * (1 - t) * t * g.cpY + t * t * g.toPos.y;
            const d = Math.sqrt((x - px) * (x - px) + (y - py) * (y - py));
            if (d < bestDist) {
                bestDist = d;
                best = g;
            }
        }
    });
    return best;
}

function hideTopologyTooltip() {
    const el = document.getElementById('mesh-topo-tooltip');
    if (el) el.classList.add('hidden');
}

function showTopologyTooltip(link, clientX, clientY) {
    const el = document.getElementById('mesh-topo-tooltip');
    if (!el || !link || !link.edges.length) {
        hideTopologyTooltip();
        return;
    }
    const lines = link.edges.map(ed => {
        const e = ed.link;
        const parts = [`${ed.from} → ${ed.to}`];
        if (e.localSeq) parts.push('#' + e.localSeq);
        if (e.srtt !== undefined && e.srtt !== null) parts.push('srtt ' + e.srtt.toFixed(1) + 'ms');
        if (e.lossRate !== undefined && e.lossRate !== null) parts.push('loss ' + (e.lossRate * 100).toFixed(1) + '%');
        if (e.retransRate > 0) parts.push('retx ' + (e.retransRate * 100).toFixed(3) + '%');
        if (e.cost !== undefined && e.cost !== null) parts.push('cost ' + e.cost.toFixed(1));
        if (e.friendlyName) parts.push(e.friendlyName);
        return parts.join(' · ');
    });
    el.textContent = lines.join('\n');
    el.classList.remove('hidden');
    const pad = 12;
    const rect = el.getBoundingClientRect();
    let left = clientX + pad;
    let top = clientY + pad;
    if (left + rect.width > window.innerWidth - 8) left = clientX - rect.width - pad;
    if (top + rect.height > window.innerHeight - 8) top = clientY - rect.height - pad;
    el.style.left = Math.max(8, left) + 'px';
    el.style.top = Math.max(8, top) + 'px';
}

function drawMeshTopology(localNodeId, peers, directPeers, fullTopology) {
    const canvas = document.getElementById('mesh-topology-canvas');
    if (!canvas) return;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;

    // Set canvas size based on container - responsive height
    const container = canvas.parentElement;
    const dpr = window.devicePixelRatio || 1;
    const width = container.clientWidth - 32;
    // Responsive height: smaller on mobile, larger on desktop
    const height = width < 600 ? 400 : 500;
    const prevWidth = _topologyState.width;
    const prevHeight = _topologyState.height;
    canvas.width = width * dpr;
    canvas.height = height * dpr;
    canvas.style.width = width + 'px';
    canvas.style.height = height + 'px';

    _topologyState.canvas = canvas;
    _topologyState.width = width;
    _topologyState.height = height;
    // A container resize invalidates the old viewport; refit once.
    if (prevWidth && (Math.abs(prevWidth - width) > 1 || Math.abs(prevHeight - height) > 1)) {
        _topologyState.autoFit = true;
    }

    // Clear in screen space. The pan/zoom transform is applied after the
    // layout step below, because auto-fit may change the viewport.
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, width, height);

    // Build node list from full topology or fallback to peers
    const allNodes = [];
    const nodeSet = new Set();

    if (fullTopology && fullTopology.nodes && fullTopology.nodes.length > 0) {
        fullTopology.nodes.forEach(n => {
            if (!nodeSet.has(n.nodeId)) {
                nodeSet.add(n.nodeId);
                allNodes.push({
                    id: n.nodeId,
                    isLocal: n.nodeId === localNodeId,
                    direct: directPeers[n.nodeId] && directPeers[n.nodeId].direct
                });
            }
        });
    } else {
        allNodes.push({ id: localNodeId, isLocal: true, direct: false });
        peers.forEach(p => {
            if (!nodeSet.has(p.nodeId)) {
                nodeSet.add(p.nodeId);
                allNodes.push({ id: p.nodeId, isLocal: false, direct: directPeers[p.nodeId] && directPeers[p.nodeId].direct });
            }
        });
    }

    syncTopoNodeSelect(allNodes);

    const centerX = width / 2;
    const centerY = height / 2;
    const nodeRadius = 20;

    // Build edge list for layout
    const edges = (fullTopology && fullTopology.edges) || [];

    // Hop-ring layout. Keep positions the user dragged; otherwise (first
    // draw, node set changed, or "reset layout") lay the graph out again.
    const positions = {};
    const allPositionsSaved = allNodes.length > 0 && allNodes.every(n => _topologyState.positions[n.id]);
    if (allPositionsSaved) {
        allNodes.forEach(node => {
            positions[node.id] = { ..._topologyState.positions[node.id] };
        });
    } else {
        Object.assign(positions, layoutHopRings(allNodes, edges, width, height, centerX, centerY));
    }

    if (_topologyState.autoFit) {
        fitTopologyView(width, height, positions);
        _topologyState.autoFit = false;
    }
    const view = _topologyState.view;
    ctx.setTransform(dpr * view.scale, 0, 0, dpr * view.scale, dpr * view.tx, dpr * view.ty);

    // Save positions for next redraw
    _topologyState.positions = positions;

    // Save current data for drag handler redraws
    _topologyState.localNodeId = allNodes.find(n => n.isLocal)?.id;
    _topologyState.peers = peers;
    _topologyState.directPeers = directPeers;
    _topologyState.fullTopology = fullTopology;

    // Metric-label placement: constant screen size, bidirectional (one per
    // direction near its source), with candidate positions along the curve and
    // perpendicular offsets so labels spread out instead of piling up.
    const labelScale = view.scale > 0 ? view.scale : 1;
    const labelFontPx = 7.5 / labelScale;
    const labelPad = 3 / labelScale;
    const labelHeight = 11 / labelScale;
    const labelStep = labelHeight + 3;
    const labelBoxes = [];
    const boxOverlapArea = (a, b) => {
        const ox = Math.min(a.x + a.w, b.x + b.w) - Math.max(a.x, b.x);
        const oy = Math.min(a.y + a.h, b.y + b.h) - Math.max(a.y, b.y);
        return ox > 0 && oy > 0 ? ox * oy : 0;
    };
    allNodes.forEach(n => {
        const p = positions[n.id];
        if (!p) return;
        const r = nodeRadius + 3;
        labelBoxes.push({ x: p.x - r, y: p.y - r, w: r * 2, h: r * 2 });
    });

    // Draw edges from full topology (reuse `edges` from layout above)
    if (edges.length > 0) {
        // Group edges by node pair first
        const nodePairEdges = {};
        edges.forEach(edge => {
            if (!edge.links || edge.links.length === 0) return;
            const pairKey = [edge.from, edge.to].sort().join('|');
            if (!nodePairEdges[pairKey]) {
                nodePairEdges[pairKey] = [];
            }
            edge.links.forEach(link => {
                nodePairEdges[pairKey].push({
                    from: edge.from,
                    to: edge.to,
                    link: link
                });
            });
        });
        
        // Within each node pair, group by matching seq numbers
        // Two edges belong to same link if one's localSeq == other's remoteSeq
        const linkGroups = [];
        Object.values(nodePairEdges).forEach(edgeList => {
            const used = new Set();
            edgeList.forEach((edge, idx) => {
                if (used.has(idx)) return;
                
                const group = {
                    friendlyName: edge.link.friendlyName || '',
                    edges: [edge]
                };
                used.add(idx);
                
                // Find matching edges (same link, opposite direction)
                const localSeq = edge.link.localSeq;
                const remoteSeq = edge.link.remoteSeq;
                
                edgeList.forEach((other, otherIdx) => {
                    if (used.has(otherIdx)) return;
                    // Match if seqs are swapped
                    if (other.link.localSeq === remoteSeq && other.link.remoteSeq === localSeq) {
                        group.edges.push(other);
                        used.add(otherIdx);
                    }
                });
                
                linkGroups.push(group);
            });
        });
        
        // Group linkGroups by node pair for drawing
        const nodePairGroups = {};
        linkGroups.forEach((group, groupIdx) => {
            if (group.edges.length === 0) return;
            const firstEdge = group.edges[0];
            const pairKey = [firstEdge.from, firstEdge.to].sort().join('|');
            if (!nodePairGroups[pairKey]) {
                nodePairGroups[pairKey] = [];
            }
            nodePairGroups[pairKey].push({ ...group, index: groupIdx });
        });
        
        // Draw each node pair group
        Object.values(nodePairGroups).forEach(groups => {
            if (groups.length === 0) return;
            
            // Get node positions (use first group's first edge)
            const firstGroup = groups[0];
            const firstEdge = firstGroup.edges[0];
            const from = positions[firstEdge.from];
            const to = positions[firstEdge.to];
            if (!from || !to) return;
            
            // Calculate edge geometry
            const dx = to.x - from.x;
            const dy = to.y - from.y;
            const len = Math.sqrt(dx * dx + dy * dy);
            if (len === 0) return;
            
            const nx = -dy / len; // perpendicular
            const ny = dx / len;
            
            groups.forEach((group, groupIndex) => {
                // Always use curves; single links get a small default offset
                const defaultOffset = (groupIndex - (groups.length - 1) / 2) * (groups.length > 1 ? 60 : 30);
                const groupKey = `${group.edges[0].from}-${group.edges[0].to}-${group.index}`;
                const curveOffset = _topologyState.lineOffsets[groupKey] !== undefined
                    ? _topologyState.lineOffsets[groupKey]
                    : defaultOffset;

                // Draw curve
                const midX = (from.x + to.x) / 2;
                const midY = (from.y + to.y) / 2;
                const cpX = midX + nx * curveOffset;
                const cpY = midY + ny * curveOffset;

                ctx.beginPath();
                ctx.moveTo(from.x, from.y);
                ctx.quadraticCurveTo(cpX, cpY, to.x, to.y);
                ctx.strokeStyle = '#3fb950';
                ctx.lineWidth = 2;
                ctx.stroke();

                // One compact label per direction near its source. Try several
                // points along the curve and perpendicular offsets so the two
                // directions of every link stay visible without piling up.
                // Full details (retx/cost/friendly) are in the hover tooltip.
                if (_topologyState.showMetrics) group.edges.forEach(e => {
                    const isFromFirst = e.from === firstEdge.from;
                    const srtt = (e.link.srtt !== undefined && e.link.srtt !== null) ? `${e.link.srtt.toFixed(0)}ms` : '?';
                    const loss = e.link.lossRate > 0 ? `${(e.link.lossRate * 100).toFixed(1)}%` : '0%';
                    const localSeq = e.link.localSeq || '';
                    if (!localSeq) return;

                    const text = `#${localSeq} ${srtt} ${loss}`;
                    ctx.font = labelFontPx + 'px -apple-system, sans-serif';
                    const m = ctx.measureText(text);
                    const ts = isFromFirst
                        ? [0.20, 0.27, 0.34, 0.41, 0.50]
                        : [0.80, 0.73, 0.66, 0.59, 0.50];
                    const offsets = [0, 1, -1, 2, -2];

                    let bestBox = null;
                    let bestOverlap = Infinity;
                    for (let ti = 0; ti < ts.length && bestOverlap > 0; ti++) {
                        const t = ts[ti];
                        const px = (1-t)*(1-t)*from.x + 2*(1-t)*t*cpX + t*t*to.x;
                        const py = (1-t)*(1-t)*from.y + 2*(1-t)*t*cpY + t*t*to.y;
                        // Tangent -> normal, so offsets move the label off the line.
                        const tx = 2*(1-t)*(cpX - from.x) + 2*t*(to.x - cpX);
                        const ty = 2*(1-t)*(cpY - from.y) + 2*t*(to.y - cpY);
                        const tlen = Math.sqrt(tx*tx + ty*ty) || 1;
                        const nx = -ty / tlen;
                        const ny = tx / tlen;
                        for (let oi = 0; oi < offsets.length; oi++) {
                            const k = offsets[oi];
                            const cx = px + nx * labelStep * k;
                            const cy = py + ny * labelStep * k;
                            const box = {
                                cx, cy,
                                x: cx - m.width / 2 - labelPad,
                                y: cy - labelHeight / 2 - labelPad,
                                w: m.width + labelPad * 2,
                                h: labelHeight + labelPad * 2
                            };
                            let overlap = 0;
                            for (let bi = 0; bi < labelBoxes.length; bi++) {
                                overlap += boxOverlapArea(box, labelBoxes[bi]);
                                if (overlap >= bestOverlap) break;
                            }
                            if (overlap < bestOverlap) {
                                bestOverlap = overlap;
                                bestBox = box;
                            }
                            if (overlap === 0) break;
                        }
                    }
                    if (!bestBox) return;
                    labelBoxes.push(bestBox);

                    ctx.fillStyle = 'rgba(0, 0, 0, 0.82)';
                    ctx.fillRect(bestBox.x, bestBox.y, bestBox.w, bestBox.h);
                    ctx.fillStyle = '#f0f6fc';
                    ctx.textAlign = 'center';
                    ctx.textBaseline = 'middle';
                    ctx.fillText(text, bestBox.cx, bestBox.cy);
                });
            });
        });
    }
    // If no edges, don't draw any connections

    // Draw nodes
    allNodes.forEach(node => {
        const pos = positions[node.id];

        // Node circle with gradient
        const gradient = ctx.createRadialGradient(pos.x - 5, pos.y - 5, 5, pos.x, pos.y, nodeRadius);
        if (node.isLocal) {
            gradient.addColorStop(0, '#79c0ff');
            gradient.addColorStop(1, '#58a6ff');
        } else if (node.direct) {
            gradient.addColorStop(0, '#3fb950');
            gradient.addColorStop(1, '#238636');
        } else {
            gradient.addColorStop(0, '#484f58');
            gradient.addColorStop(1, '#30363d');
        }

        ctx.beginPath();
        ctx.arc(pos.x, pos.y, nodeRadius, 0, 2 * Math.PI);
        ctx.fillStyle = gradient;
        ctx.fill();
        ctx.strokeStyle = node.isLocal ? '#79c0ff' : '#484f58';
        ctx.lineWidth = 2.5;
        ctx.stroke();

        // Locate highlight (kept until another node is selected / relayout).
        if (_topologyState.focusNode === node.id) {
            ctx.beginPath();
            ctx.arc(pos.x, pos.y, nodeRadius + 6, 0, 2 * Math.PI);
            ctx.strokeStyle = '#f0b72f';
            ctx.lineWidth = 2;
            ctx.stroke();
        }

        // Node label - truncate if too long for the circle
        ctx.fillStyle = '#ffffff';
        ctx.font = 'bold 12px -apple-system, sans-serif';
        ctx.textAlign = 'center';
        ctx.textBaseline = 'middle';
        const label = node.id;
        const maxWidth = nodeRadius * 1.8;
        const measured = ctx.measureText(label);
        if (measured.width <= maxWidth) {
            ctx.fillText(label, pos.x, pos.y);
        } else {
            // Truncate with ellipsis
            let truncated = label;
            while (truncated.length > 1 && ctx.measureText(truncated + '…').width > maxWidth) {
                truncated = truncated.slice(0, -1);
            }
            ctx.fillText(truncated + '…', pos.x, pos.y);
            // Show full ID below the node
            ctx.font = '10px -apple-system, sans-serif';
            ctx.fillStyle = '#8b949e';
            ctx.fillText(label, pos.x, pos.y + nodeRadius + 12);
        }
    });

    // Setup drag handlers if not already done
    if (!canvas._dragHandlersAttached) {
        canvas._dragHandlersAttached = true;
        
        canvas.addEventListener('mousedown', function(e) {
            hideTopologyTooltip();
            const rect = canvas.getBoundingClientRect();
            const view = _topologyState.view;
            const sx = e.clientX - rect.left;
            const sy = e.clientY - rect.top;
            // Hit-testing and node dragging are in world coordinates.
            const x = (sx - view.tx) / view.scale;
            const y = (sy - view.ty) / view.scale;

            // Middle mouse always pans, regardless of what is under the cursor.
            if (e.button === 1) {
                _topologyState.panning = { startX: e.clientX, startY: e.clientY, tx: view.tx, ty: view.ty, moved: false };
                canvas.classList.add('is-panning');
                e.preventDefault();
                return;
            }

            // First, check if clicking on a node
            let clickedNode = null;
            for (const nodeId in _topologyState.positions) {
                const pos = _topologyState.positions[nodeId];
                const dx = x - pos.x;
                const dy = y - pos.y;
                if (dx * dx + dy * dy <= nodeRadius * nodeRadius) {
                    clickedNode = nodeId;
                    break;
                }
            }
            
            if (clickedNode) {
                // Drag node
                _topologyState.dragging = clickedNode;
                _topologyState.dragOffset = { x: x - _topologyState.positions[clickedNode].x, y: y - _topologyState.positions[clickedNode].y };
                _topologyState.dragStartPos = { x: e.clientX, y: e.clientY };
                canvas.style.cursor = 'grabbing';
            } else if (e.shiftKey || e.altKey) {
                // Check if clicking on a line. Curve adjustment is a modifier
                // action so a plain drag on empty canvas always pans.
                const fullTopology = _topologyState.fullTopology;
                if (fullTopology && fullTopology.edges) {
                    const positions = _topologyState.positions;
                    let closestLine = null;
                    let minDist = 10 / view.scale;  // 10 screen px, expressed in world units
                    
                    // Group edges by node pair and seq matching (same logic as drawing)
                    const nodePairEdges = {};
                    fullTopology.edges.forEach(edge => {
                        if (!edge.links || edge.links.length === 0) return;
                        const pairKey = [edge.from, edge.to].sort().join('|');
                        if (!nodePairEdges[pairKey]) {
                            nodePairEdges[pairKey] = [];
                        }
                        edge.links.forEach(link => {
                            nodePairEdges[pairKey].push({ from: edge.from, to: edge.to, link });
                        });
                    });
                    
                    // Within each node pair, group by matching seq numbers
                    const linkGroups = [];
                    Object.values(nodePairEdges).forEach(edgeList => {
                        const used = new Set();
                        edgeList.forEach((edge, idx) => {
                            if (used.has(idx)) return;
                            const group = { edges: [edge] };
                            used.add(idx);
                            const localSeq = edge.link.localSeq;
                            const remoteSeq = edge.link.remoteSeq;
                            edgeList.forEach((other, otherIdx) => {
                                if (used.has(otherIdx)) return;
                                if (other.link.localSeq === remoteSeq && other.link.remoteSeq === localSeq) {
                                    group.edges.push(other);
                                    used.add(otherIdx);
                                }
                            });
                            linkGroups.push(group);
                        });
                    });
                    
                    // Group by node pair for distance checking
                    const nodePairGroups = {};
                    linkGroups.forEach((group, groupIdx) => {
                        if (group.edges.length === 0) return;
                        const firstEdge = group.edges[0];
                        const pairKey = [firstEdge.from, firstEdge.to].sort().join('|');
                        if (!nodePairGroups[pairKey]) {
                            nodePairGroups[pairKey] = [];
                        }
                        nodePairGroups[pairKey].push({ ...group, index: groupIdx });
                    });
                    
                    // Check distance to each line
                    Object.values(nodePairGroups).forEach(groups => {
                        if (groups.length === 0) return;
                        const firstGroup = groups[0];
                        const firstEdge = firstGroup.edges[0];
                        const from = positions[firstEdge.from];
                        const to = positions[firstEdge.to];
                        if (!from || !to) return;
                        
                        const dx = to.x - from.x;
                        const dy = to.y - from.y;
                        const len = Math.sqrt(dx * dx + dy * dy);
                        if (len === 0) return;
                        
                        const nx = -dy / len;
                        const ny = dx / len;

                        groups.forEach((group, groupIndex) => {
                            const defaultOffset = (groupIndex - (groups.length - 1) / 2) * (groups.length > 1 ? 60 : 30);
                            const groupKey = `${group.edges[0].from}-${group.edges[0].to}-${group.index}`;
                            const curveOffset = _topologyState.lineOffsets[groupKey] !== undefined
                                ? _topologyState.lineOffsets[groupKey]
                                : defaultOffset;

                            // Sample points along the quadratic curve
                            const midX = (from.x + to.x) / 2;
                            const midY = (from.y + to.y) / 2;
                            const cpX = midX + nx * curveOffset;
                            const cpY = midY + ny * curveOffset;
                            let dist = Infinity;
                            for (let t = 0; t <= 1; t += 0.1) {
                                const px = (1-t)*(1-t)*from.x + 2*(1-t)*t*cpX + t*t*to.x;
                                const py = (1-t)*(1-t)*from.y + 2*(1-t)*t*cpY + t*t*to.y;
                                const d = Math.sqrt((x - px) * (x - px) + (y - py) * (y - py));
                                if (d < dist) dist = d;
                            }
                            
                            if (dist < minDist) {
                                minDist = dist;
                                closestLine = {
                                    groupKey: groupKey,
                                    from: firstEdge.from,
                                    to: firstEdge.to,
                                    startOffset: curveOffset,
                                    startX: x,
                                    startY: y,
                                    nx, ny
                                };
                            }
                        });
                    });
                    
                    if (closestLine) {
                        // Start dragging line
                        _topologyState.draggingLine = closestLine;
                        _topologyState.dragStartPos = { x: e.clientX, y: e.clientY };
                        canvas.style.cursor = 'grabbing';
                    }
                }
            }

            // Empty canvas (no node / line hit): drag to pan the viewport.
            if (!_topologyState.dragging && !_topologyState.draggingLine) {
                _topologyState.panning = { startX: e.clientX, startY: e.clientY, tx: view.tx, ty: view.ty, moved: false };
                canvas.classList.add('is-panning');
            }
        });

        canvas.addEventListener('mousemove', function(e) {
            const view = _topologyState.view;
            if (_topologyState.panning) {
                const p = _topologyState.panning;
                view.tx = p.tx + (e.clientX - p.startX);
                view.ty = p.ty + (e.clientY - p.startY);
                if (Math.abs(e.clientX - p.startX) + Math.abs(e.clientY - p.startY) > 3) p.moved = true;
                redrawMeshTopology();
                return;
            }
            if (_topologyState.dragging) {
                // Dragging node
                const rect = canvas.getBoundingClientRect();
                const x = (e.clientX - rect.left - view.tx) / view.scale;
                const y = (e.clientY - rect.top - view.ty) / view.scale;
                _topologyState.positions[_topologyState.dragging] = {
                    x: x - _topologyState.dragOffset.x,
                    y: y - _topologyState.dragOffset.y
                };
                // Redraw using saved state (avoids stale closure)
                redrawMeshTopology();
            } else if (_topologyState.draggingLine) {
                // Dragging line
                const rect = canvas.getBoundingClientRect();
                const x = (e.clientX - rect.left - view.tx) / view.scale;
                const y = (e.clientY - rect.top - view.ty) / view.scale;

                // Calculate perpendicular displacement
                const dx = x - _topologyState.draggingLine.startX;
                const dy = y - _topologyState.draggingLine.startY;
                const perpDist = dx * _topologyState.draggingLine.nx + dy * _topologyState.draggingLine.ny;

                // Update offset
                _topologyState.lineOffsets[_topologyState.draggingLine.groupKey] =
                    _topologyState.draggingLine.startOffset + perpDist;

                // Redraw
                redrawMeshTopology();
            } else {
                // Change cursor on hover
                const rect = canvas.getBoundingClientRect();
                const x = (e.clientX - rect.left - view.tx) / view.scale;
                const y = (e.clientY - rect.top - view.ty) / view.scale;
                let overNode = false;
                for (const nodeId in _topologyState.positions) {
                    const pos = _topologyState.positions[nodeId];
                    const dx = x - pos.x;
                    const dy = y - pos.y;
                    if (dx * dx + dy * dy <= nodeRadius * nodeRadius) {
                        overNode = true;
                        break;
                    }
                }
                if (overNode) {
                    hideTopologyTooltip();
                    canvas.style.cursor = 'pointer';
                } else {
                    const link = findTopologyLinkAt(x, y, 8 / view.scale);
                    if (link) {
                        showTopologyTooltip(link, e.clientX, e.clientY);
                        canvas.style.cursor = 'help';
                    } else {
                        hideTopologyTooltip();
                        canvas.style.cursor = 'grab';
                    }
                }
            }
        });

        canvas.addEventListener('mouseup', function(e) {
            // Check if it was a click (not a drag)
            if (_topologyState.dragging && _topologyState.dragStartPos) {
                const dx = e.clientX - _topologyState.dragStartPos.x;
                const dy = e.clientY - _topologyState.dragStartPos.y;
                const moved = dx * dx + dy * dy;
                // If didn't move much, treat as click
                if (moved < 25) {
                    const nodeId = _topologyState.dragging;
                    const localId = _topologyState.localNodeId;
                    if (nodeId && nodeId !== localId) {
                        // Open the node's admin page in new tab
                        const url = 'https://' + nodeId + '.phn/';
                        window.open(url, '_blank');
                    }
                }
            }
            _topologyState.dragging = null;
            _topologyState.draggingLine = null;
            _topologyState.dragStartPos = null;
            _topologyState.panning = null;
            canvas.classList.remove('is-panning');
            canvas.style.cursor = 'grab';
        });

        canvas.addEventListener('mouseleave', function() {
            _topologyState.dragging = null;
            _topologyState.draggingLine = null;
            _topologyState.dragStartPos = null;
            _topologyState.panning = null;
            canvas.classList.remove('is-panning');
            canvas.style.cursor = '';
            hideTopologyTooltip();
        });

        // Wheel zoom anchored at the cursor.
        canvas.addEventListener('wheel', function(e) {
            e.preventDefault();
            hideTopologyTooltip();
            const rect = canvas.getBoundingClientRect();
            const factor = e.deltaY < 0 ? 1.12 : 1 / 1.12;
            meshTopoZoomAt(factor, e.clientX - rect.left, e.clientY - rect.top);
        }, { passive: false });

        // Double-click to reset layout
        canvas.addEventListener('dblclick', function() {
            meshTopoRelayout();
        });

        // Touch: same semantics as the mouse (drag a node, drag empty space to
        // pan, tap a node to open its console).
        canvas.addEventListener('touchstart', function(e) {
            if (e.touches.length !== 1) return;
            const touch = e.touches[0];
            const rect = canvas.getBoundingClientRect();
            const view = _topologyState.view;
            const x = (touch.clientX - rect.left - view.tx) / view.scale;
            const y = (touch.clientY - rect.top - view.ty) / view.scale;

            for (const nodeId in _topologyState.positions) {
                const pos = _topologyState.positions[nodeId];
                const dx = x - pos.x;
                const dy = y - pos.y;
                if (dx * dx + dy * dy <= nodeRadius * nodeRadius) {
                    _topologyState.dragging = nodeId;
                    _topologyState.dragOffset = { x: dx, y: dy };
                    _topologyState.dragStartPos = { x: touch.clientX, y: touch.clientY };
                    e.preventDefault();
                    return;
                }
            }
            _topologyState.panning = {
                startX: touch.clientX, startY: touch.clientY,
                tx: view.tx, ty: view.ty, moved: false
            };
        }, { passive: false });

        canvas.addEventListener('touchmove', function(e) {
            if (e.touches.length !== 1) return;
            const touch = e.touches[0];
            e.preventDefault();
            const view = _topologyState.view;
            if (_topologyState.panning) {
                const p = _topologyState.panning;
                view.tx = p.tx + (touch.clientX - p.startX);
                view.ty = p.ty + (touch.clientY - p.startY);
                if (Math.abs(touch.clientX - p.startX) + Math.abs(touch.clientY - p.startY) > 3) p.moved = true;
                redrawMeshTopology();
                return;
            }
            if (_topologyState.dragging) {
                const rect = canvas.getBoundingClientRect();
                const x = (touch.clientX - rect.left - view.tx) / view.scale;
                const y = (touch.clientY - rect.top - view.ty) / view.scale;
                _topologyState.positions[_topologyState.dragging] = {
                    x: x - _topologyState.dragOffset.x,
                    y: y - _topologyState.dragOffset.y
                };
                redrawMeshTopology();
            }
        }, { passive: false });

        canvas.addEventListener('touchend', function(e) {
            if (_topologyState.dragging && _topologyState.dragStartPos) {
                const touch = e.changedTouches[0];
                const dx = touch.clientX - _topologyState.dragStartPos.x;
                const dy = touch.clientY - _topologyState.dragStartPos.y;
                const moved = dx * dx + dy * dy;
                if (moved < 25) {
                    const nodeId = _topologyState.dragging;
                    const localId = _topologyState.localNodeId;
                    if (nodeId && nodeId !== localId) {
                        window.open('https://' + nodeId + '.phn/', '_blank');
                    }
                }
            }
            _topologyState.dragging = null;
            _topologyState.dragStartPos = null;
            _topologyState.panning = null;
        });

        // Resize observer for responsive canvas
        if (!canvas._resizeObserverAttached && typeof ResizeObserver !== 'undefined') {
            canvas._resizeObserverAttached = true;
            const resizeObserver = new ResizeObserver(function() {
                if (_topologyState.localNodeId) {
                    // Refit the viewport; keep user-dragged node positions.
                    _topologyState.autoFit = true;
                    redrawMeshTopology();
                }
            });
            resizeObserver.observe(canvas.parentElement);
        }
    }
}

function timeAgo(date) {
    const seconds = Math.floor((new Date() - date) / 1000);
    if (seconds < 10) return i18n.t('time.justNow');
    if (seconds < 60) return i18n.t('time.secondsAgo').replace('{}', seconds);
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return i18n.t('time.minutesAgo').replace('{}', minutes);
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return i18n.t('time.hoursAgo').replace('{}', hours);
    const days = Math.floor(hours / 24);
    return i18n.t('time.daysAgo').replace('{}', days);
}

function escapeHtml(s) {
    if (!s) return '';
    return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

async function triggerMeshGossip() {
    const btn = event.target;
    await withButtonLoading(btn, async () => {
        try {
            const resp = await fetch('./api/mesh/gossip', {method: 'POST'});
            if (resp.ok) {
                showToast(i18n.t('mesh.gossipTriggered'), 'success');
            } else {
                showToast(i18n.t('mesh.gossipFailed'), 'error');
            }
        } catch (err) {
            showToast(i18n.t('mesh.gossipError') + err.message, 'error');
        }
    });
}

let connLogLastSeq = 0;
let connLogBoot = null;         // server process identity; change => restart, cursor invalid

// ===== Generic block collapse =====
// Any header element carrying data-collapse toggles the sibling blocks after
// it (or a single body via data-collapse-target="id") on click. Buttons,
// links and inputs inside the header keep their own handlers. State persists
// per header id in localStorage so htmx page swaps restore it.
(function initCollapsible() {
    document.addEventListener('click', function (ev) {
        const header = ev.target.closest('[data-collapse]');
        if (!header) return;
        if (ev.target.closest('button, a, input, select, textarea, label')) return;
        toggleCollapse(header);
    });
    document.addEventListener('htmx:afterSwap', applyPersistedCollapse);
    if (document.readyState !== 'loading') applyPersistedCollapse();
    else document.addEventListener('DOMContentLoaded', applyPersistedCollapse);
})();

function collapseKey(header) {
    const host = header.closest('[id]');
    return host && host.id ? 'collapse:' + location.pathname + ':' + host.id : null;
}

function collapseBodies(header) {
    const target = header.dataset.collapseTarget;
    if (target) {
        const el = document.getElementById(target);
        return el ? [el] : [];
    }
    const bodies = [];
    let el = header.nextElementSibling;
    while (el) { bodies.push(el); el = el.nextElementSibling; }
    return bodies;
}

function setCollapse(header, bodies, hidden) {
    bodies.forEach(b => { b.style.display = hidden ? 'none' : ''; });
    header.dataset.collapsed = hidden ? '1' : '0';
    const icon = header.querySelector('.collapse-icon');
    if (icon) icon.style.transform = hidden ? 'rotate(0deg)' : 'rotate(90deg)';
    if (!hidden) lazyLoadCollapse(header);
}

// Sections marked data-collapse-load fetch their data on first expand
// (including expand restored from localStorage), not at page load.
function lazyLoadCollapse(header) {
    const fnName = header.dataset.collapseLoad;
    if (!fnName || header.dataset.collapseLoaded === '1') return;
    const fn = window[fnName];
    if (typeof fn !== 'function') return;
    header.dataset.collapseLoaded = '1';
    try { fn(); } catch (e) { console.error('collapse-load failed:', fnName, e); }
}

function toggleCollapse(header) {
    const bodies = collapseBodies(header);
    const hidden = header.dataset.collapsed !== '1';
    setCollapse(header, bodies, hidden);
    const key = collapseKey(header);
    if (key) { try { localStorage.setItem(key, hidden ? '1' : '0'); } catch (e) { /* private mode */ } }
}

function applyPersistedCollapse() {
    document.querySelectorAll('[data-collapse]').forEach(header => {
        const key = collapseKey(header);
        let saved = null;
        if (key) { try { saved = localStorage.getItem(key); } catch (e) { /* ignore */ } }
        const hidden = saved === null ? header.dataset.collapsed === '1' : saved === '1';
        setCollapse(header, collapseBodies(header), hidden);
    });
}

// ===== Active Connections =====
var activeConnsMap = new Map();
var activeConnsRows = new Map(); // conn id -> rendered <tr>
var activeConnsTable = null;     // persistent <table>, tbody reused across renders
var activeConnsLastSeq = 0;
var activeConnsBoot = null;      // server process identity; change => restart, cursor invalid
var activeConnsTimer = null;
var activeConnFilter = '';       // lowercase text filter across row cells

// Open the logs view with a preset correlation filter. The preset rides to
// the new window through sessionStorage (copied at open time for popups).
function openLogsWithPreset(preset) {
    try { sessionStorage.setItem('connLogPreset', JSON.stringify(preset)); } catch (e) { /* private mode */ }
    window.open('./logs', 'PhaethonLogs');
}

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

        if (data.boot !== activeConnsBoot) {
            // Server process restarted: our seq cursor belongs to the old
            // process and journal seqs have restarted from zero. Discard
            // everything and pull a full snapshot.
            const hadCursor = activeConnsLastSeq > 0;
            activeConnsBoot = data.boot;
            activeConnsMap.clear();
            activeConnsLastSeq = 0;
            if (hadCursor) {
                fetchActiveConns(false);
                return;
            }
        }

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

    // htmx menu navigation destroys #main-content DOM: a previously built
    // table/rows may be detached from the document. Holding on to them
    // would render into nowhere and leave the placeholder visible, so
    // drop stale references and rebuild against the live element.
    if (activeConnsTable && !activeConnsTable.isConnected) {
        activeConnsTable = null;
        activeConnsRows.clear();
    }

    const conns = Array.from(activeConnsMap.values());
    conns.forEach(c => { c._startMs = +new Date(c.startTime); });
    conns.sort((a, b) => a._startMs - b._startMs);

    if (conns.length === 0) {
        if (activeConnsTable) {
            activeConnsTable.remove();
            activeConnsTable = null;
            activeConnsRows.clear();
        }
        el.innerHTML = '<p class="text-muted" data-i18n="dash.noActiveConns">' + i18n.t('dash.noActiveConns') + '</p>';
    } else {
        if (!activeConnsTable) {
            const table = document.createElement('table');
            table.className = 'data-table';
            table.style.fontSize = '0.85rem';
            const thead = document.createElement('thead');
            const headTr = document.createElement('tr');
            [['dash.connProtocol', 'Protocol'], ['dash.connInbound', 'Inbound'], ['dash.connSrc', 'Source'], ['dash.connDst', 'Destination'], ['dash.connRule', 'Rule'], ['dash.connDuration', 'Duration'], ['dash.connActions', '']].forEach(([key, label]) => {
                const th = document.createElement('th');
                th.setAttribute('data-i18n', key);
                th.textContent = i18n.t(key);
                th.dataset.label = label;
                headTr.appendChild(th);
            });
            thead.appendChild(headTr);
            const tbody = document.createElement('tbody');
            table.appendChild(thead);
            table.appendChild(tbody);
            el.innerHTML = '';
            el.appendChild(table);
            activeConnsTable = table;
        }
        const tbody = activeConnsTable.tBodies[0];

        // Drop rows for connections that no longer exist.
        const seen = new Set();
        conns.forEach(c => seen.add(c.id));
        for (const [id, tr] of activeConnsRows) {
            if (!seen.has(id)) {
                tr.remove();
                activeConnsRows.delete(id);
            }
        }

        // Create rows for new connections; cell text via textContent (no HTML injection risk).
        conns.forEach(c => {
            let tr = activeConnsRows.get(c.id);
            if (!tr) {
                tr = document.createElement('tr');
                ['Protocol', 'Inbound', 'Source', 'Destination', 'Rule'].forEach(label => {
                    const td = document.createElement('td');
                    td.dataset.label = label;
                    tr.appendChild(td);
                });
                const durTd = document.createElement('td');
                durTd.dataset.label = 'Duration';
                tr.appendChild(durTd);
                const actTd = document.createElement('td');
                actTd.dataset.label = 'Actions';
                const chainBtn = document.createElement('button');
                chainBtn.className = 'conn-chain-btn';
                chainBtn.textContent = '⇄';
                chainBtn.title = i18n.t('dash.connChain');
                chainBtn.style.display = c.fwdKey ? '' : 'none';
                chainBtn.addEventListener('click', ev => {
                    ev.stopPropagation();
                    openLogsWithPreset({ kind: 'fwdKey', value: c.fwdKey });
                });
                actTd.appendChild(chainBtn);
                tr.appendChild(actTd);
                tr.addEventListener('click', () => openLogsWithPreset({ kind: 'id', value: c.id }));
                tr.title = activeConnTooltip(c);
                const src = c.srcAddr || '';
                const dst = c.dstAddr + ':' + c.dstPort;
                tr.children[0].textContent = c.protocol || '';
                tr.children[1].textContent = c.inbound || '';
                tr.children[2].textContent = src;
                if ((c.inbound || '') === 'TUN') {
                    // TUN flows carry no process info; the source is a
                    // data-plane view (restored original IP when known).
                    tr.children[2].classList.add('conn-src-dim');
                    tr.children[2].title = i18n.t('dash.tunSrcHint');
                }
                tr.children[3].textContent = dst;
                tr.children[3].title = [c.originalDstAddr, c.dstAddr + ':' + c.dstPort].filter(Boolean).join(' → ');
                tr.children[4].textContent = egressLabel(c);
                durTd.dataset.start = String(c._startMs);
                activeConnsRows.set(c.id, tr);
            } else {
                const durTd = tr.children[5];
                if (durTd.dataset.start !== String(c._startMs)) {
                    durTd.dataset.start = String(c._startMs);
                }
            }
        });

        // Reorder: appendChild on an existing node moves it, so a single pass
        // in sorted order yields the correct final order without rebuilding.
        conns.forEach(c => tbody.appendChild(activeConnsRows.get(c.id)));

        // Client-side text filter across all cells.
        if (activeConnFilter) {
            activeConnsRows.forEach((tr, id) => {
                const c = activeConnsMap.get(id);
                const hay = [c.protocol, c.inbound, c.srcAddr, c.originalDstAddr, c.dstAddr, c.dstPort, c.rule, c.proxy, c.actualProxy, c.via, c.id].filter(Boolean).join(' ').toLowerCase();
                tr.style.display = hay.includes(activeConnFilter) ? '' : 'none';
            });
        } else {
            activeConnsRows.forEach(tr => { tr.style.display = ''; });
        }

        const now = Date.now();
        activeConnsRows.forEach(tr => {
            tr.children[5].textContent = formatDuration(now - +tr.children[5].dataset.start);
        });
    }

    if (countEl) {
        countEl.textContent = i18n.t('dash.activeConnCount').replace('{}', conns.length);
    }
}

// Full-field tooltip for an active-connection row: everything the list
// columns compress, in original form.
function activeConnTooltip(c) {
    const lines = [
        i18n.t('dash.ttConnId') + ': ' + c.id,
        i18n.t('dash.connProtocol') + ': ' + (c.protocol || ''),
        i18n.t('dash.connInbound') + ': ' + (c.inbound || ''),
        i18n.t('dash.ttOrigDst') + ': ' + (c.originalDstAddr || '-'),
        i18n.t('dash.connDst') + ': ' + c.dstAddr + ':' + c.dstPort,
        i18n.t('dash.connRule') + ': ' + (c.rule || '-'),
        i18n.t('dash.connProxy') + ': ' + (c.proxy || '-'),
    ];
    if (c.actualProxy) lines.push(i18n.t('dash.ttActualProxy') + ': ' + c.actualProxy);
    if (c.via) lines.push(i18n.t('dash.ttVia') + ': ' + c.via);
    if (c.mapping) lines.push(i18n.t('dash.ttMapping') + ': ' + c.mapping);
    if (c.timeRange) lines.push(i18n.t('dash.ttTimeRange') + ': ' + c.timeRange);
    if (c.fwdKey) lines.push(i18n.t('dash.ttFwdKey') + ': ' + c.fwdKey);
    return lines.join('\n');
}

function setActiveConnFilter(query) {
    activeConnFilter = query.toLowerCase();
    renderActiveConns();
}

function startActiveConnsTimer() {
    if (activeConnsTimer) return;
    activeConnsTimer = setInterval(() => {
        const now = Date.now();
        document.querySelectorAll('#active-conns-list td[data-start]').forEach(td => {
            td.textContent = formatDuration(now - +td.dataset.start);
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
            tbody tr { cursor:pointer; }
            tbody tr:hover { background:#2a2a4e; }
            tbody tr.selected { background:#4a9eff; color:#fff; }
            tbody tr.selected:hover { background:#3a8eef; }
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
        let pipBoot = null;
        let pipSelectedConnId = null;

        function renderPipConns() {
            const conns = Array.from(pipConnsMap.values());
            conns.sort((a, b) => new Date(a.startTime) - new Date(b.startTime));
            if (conns.length === 0) {
                contentEl.innerHTML = '<p>' + i18n.t('connpip.noConns') + '</p>';
            } else {
                let html = '<table><thead><tr>';
                html += '<th>' + i18n.t('dash.connProtocol') + '</th>';
                html += '<th>' + i18n.t('dash.connInbound') + '</th>';
                html += '<th>' + i18n.t('dash.connDst') + '</th>';
                html += '<th>' + i18n.t('dash.connRule') + '</th>';
                html += '<th>' + i18n.t('dash.connDuration') + '</th>';
                html += '</tr></thead><tbody>';
                conns.forEach(c => {
                    const dur = formatDuration(Date.now() - new Date(c.startTime).getTime());
                    const dst = c.dstAddr + ':' + c.dstPort;
                    const rule = egressLabel(c);
                    const inbound = c.inbound || '';
                    const selected = c.id === pipSelectedConnId ? ' class="selected"' : '';
                    html += '<tr' + selected + ' data-conn-id="' + c.id + '"><td>' + c.protocol + '</td><td>' + inbound + '</td><td>' + dst + '</td><td>' + rule + '</td><td data-start="' + c.startTime + '">' + dur + '</td></tr>';
                });
                html += '</tbody></table>';
                contentEl.innerHTML = html;
                contentEl.querySelectorAll('tbody tr').forEach(tr => {
                    tr.onclick = () => {
                        pipSelectedConnId = pipSelectedConnId === tr.dataset.connId ? null : tr.dataset.connId;
                        contentEl.querySelectorAll('tbody tr').forEach(r => {
                            if (r.dataset.connId === pipSelectedConnId) {
                                r.classList.add('selected');
                            } else {
                                r.classList.remove('selected');
                            }
                        });
                    };
                });
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
                if (data.boot !== pipBoot) {
                    // Server restarted: discard cursor and resync full.
                    const hadCursor = pipLastSeq > 0;
                    pipBoot = data.boot;
                    pipConnsMap.clear();
                    pipLastSeq = 0;
                    if (hadCursor) {
                        fetchPipConns();
                        return;
                    }
                }
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

const MAX_CONN_LOG_LINES = 500;
var connLogLines = [];       // model: {text, err, connId, fwdKey}, oldest first
var connLogFilterQuery = ''; // active filter (lowercased), '' = show all
var connLogErrorsOnly = false;
// Cross-page preset set by the connections page: {kind:'id'|'fwdKey', value}.
// Consumed by the logs page on load (passed via sessionStorage copy at open).
var connLogPreset = null;

// Egress display label shared by log lines, active-conn rows and the PiP
// popup: mesh-routed flows show the egress node, DIRECT shows 直连, LOCAL
// stays LOCAL, everything else shows rule → actualProxy.
function egressLabel(e) {
    if (e.via) return i18n.t('dash.egressMesh').replace('{}', e.via);
    const proxy = e.proxy || '';
    if (proxy === 'DIRECT') return i18n.t('dash.egressDirect');
    let label = e.rule || proxy || 'DIRECT';
    if (e.actualProxy && e.actualProxy !== e.proxy) {
        label += ' → ' + e.actualProxy;
    }
    return label;
}

function connLogFormatEntry(e) {
    const date = new Date(e.time);
    const now = new Date();
    const pad = n => String(n).padStart(2, '0');
    const ms = String(date.getMilliseconds()).padStart(3, '0');
    // Cross-day stamp: once a log line is from another day, HH:MM:SS alone
    // is ambiguous — prefix MM-DD.
    const time = (date.toDateString() !== now.toDateString())
        ? `${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}.${ms}`
        : `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}.${ms}`;
    const icon = e.status === 'ok' ? '✓' : '✗';
    const inbound = e.inbound || '';
    const src = e.srcAddr || '';
    const target = egressLabel(e);
    let line = `${icon} [${inbound}]`;
    if (e.connId) line += ` [${e.connId}]`;
    line += ` ${e.protocol} ${src} → ${e.dstAddr}:${e.dstPort} → ${target}`;
    if (e.error) line += ` (${e.error})`;
    return {
        text: `[${time}] ${line}`,
        err: e.status !== 'ok',
        connId: e.connId || '',
        fwdKey: e.fwdKey || '',
    };
}

function connLogLineVisible(l) {
    if (connLogErrorsOnly && !l.err) return false;
    if (connLogFilterQuery && !l.text.toLowerCase().includes(connLogFilterQuery)) return false;
    return true;
}

// Full rebuild of the <pre> from the model. Used on first load, filter
// changes, and any time incremental node surgery doesn't apply.
function connLogRenderAll(el) {
    const visible = connLogLines.filter(connLogLineVisible);
    el.textContent = '';
    if (visible.length === 0) {
        el.textContent = (connLogFilterQuery || connLogErrorsOnly) ? 'No matching logs' : 'No logs yet';
        return;
    }
    const frag = document.createDocumentFragment();
    visible.forEach(l => {
        const div = document.createElement('div');
        div.textContent = l.text;
        if (l.err) div.className = 'conn-log-err';
        frag.appendChild(div);
    });
    el.appendChild(frag);
}

// Append new lines to the model and patch the DOM incrementally: remove
// trimmed head nodes, append one div per new line. Falls back to a full
// rebuild when a filter is active or the element has no line nodes yet.
function connLogAppend(el, lines) {
    connLogLines.push(...lines);
    let trimmed = 0;
    if (connLogLines.length > MAX_CONN_LOG_LINES) {
        trimmed = connLogLines.length - MAX_CONN_LOG_LINES;
        connLogLines.splice(0, trimmed);
    }
    if (connLogFilterQuery || connLogErrorsOnly || el.firstElementChild === null) {
        connLogRenderAll(el);
        return;
    }
    for (let i = 0; i < trimmed && el.firstChild; i++) {
        el.removeChild(el.firstChild);
    }
    const frag = document.createDocumentFragment();
    lines.forEach(l => {
        const div = document.createElement('div');
        div.textContent = l.text;
        if (l.err) div.className = 'conn-log-err';
        frag.appendChild(div);
    });
    el.appendChild(frag);
}

// Filter entry point for the logs page.
function setConnLogFilter(query) {
    connLogFilterQuery = query.toLowerCase();
    connLogUpdateChip();
    const el = document.getElementById('conn-logs');
    if (el) {
        connLogRenderAll(el);
        el.scrollTop = el.scrollHeight;
    }
}

function setConnLogErrorsOnly(on) {
    connLogErrorsOnly = !!on;
    const el = document.getElementById('conn-logs');
    if (el) {
        connLogRenderAll(el);
        el.scrollTop = el.scrollHeight;
    }
}

// Active-preset chip shown above the log view: connId text filter or a
// server-side fwdKey chain filter. Clear restores the full stream.
function connLogUpdateChip() {
    const chipEl = document.getElementById('log-active-filter');
    if (!chipEl) return;
    const parts = [];
    if (connLogFilterQuery) parts.push(i18n.t('logs.chipConnId') + ': ' + connLogFilterQuery);
    if (connLogPreset && connLogPreset.kind === 'fwdKey') parts.push(i18n.t('logs.chipFwdKey') + ': ' + connLogPreset.value);
    if (parts.length === 0) {
        chipEl.style.display = 'none';
        return;
    }
    chipEl.style.display = '';
    chipEl.textContent = parts.join('  |  ') + '  ✕';
}

async function fetchConnections(incremental) {
    const el = document.getElementById('conn-logs');
    if (!el) return;
    try {
        let url = './api/connections';
        const params = [];
        if (incremental && connLogLastSeq > 0 && !(connLogPreset && connLogPreset.kind === 'fwdKey')) {
            params.push('after=' + connLogLastSeq);
        }
        if (connLogPreset && connLogPreset.kind === 'fwdKey') {
            // Server-side chain filter: full snapshot each time, no seq cursor.
            params.push('fwdKey=' + encodeURIComponent(connLogPreset.value));
        }
        if (params.length) url += '?' + params.join('&');
        const res = await fetch(url);
        if (!res.ok) throw new Error('status ' + res.status);
        const data = await res.json();
        if (data.boot !== connLogBoot) {
            // Server restarted: seq cursor belongs to the old process.
            const hadCursor = connLogLastSeq > 0;
            connLogBoot = data.boot;
            connLogLastSeq = 0;
            if (hadCursor) {
                fetchConnections(false);
                return;
            }
        }
        // data.stale: full snapshot (first load, or server restarted and
        // our seq cursor predates the reset). Reset before appending so
        // incremental mode never duplicates lines.
        if (data.stale) {
            connLogLines = [];
            el.textContent = '';
        }
        if (!data.logs || data.logs.length === 0) {
            if (data.stale) connLogRenderAll(el);
            return;
        }
        connLogAppend(el, data.logs.map(connLogFormatEntry));
        connLogLastSeq = data.logs[data.logs.length - 1].seq;
        // Only auto-scroll if checkbox is checked
        const autoScrollCheckbox = document.getElementById('log-auto-scroll');
        if (!autoScrollCheckbox || autoScrollCheckbox.checked) {
            el.scrollTop = el.scrollHeight;
        }
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
                .pip-filter-bar {
                    background: #161b22;
                    border-bottom: 1px solid #30363d;
                    padding: 8px 20px;
                    display: flex;
                    gap: 10px;
                    align-items: center;
                    flex-shrink: 0;
                    flex-wrap: wrap;
                }
                .pip-filter-bar select, .pip-filter-bar input {
                    padding: 5px 10px;
                    border-radius: 5px;
                    border: 1px solid #30363d;
                    background: #0d1117;
                    color: #c9d1d9;
                    font-size: 13px;
                }
                .pip-filter-bar select { min-width: 100px; }
                .pip-filter-bar input { flex: 1; min-width: 150px; }
                .pip-filter-bar label {
                    font-size: 12px;
                    color: #8b949e;
                    margin-right: 4px;
                }
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
                <div class="pip-filter-bar">
                    <label data-i18n="logs.filterStatus">状态</label>
                    <select id="pip-filter-status">
                        <option value="" data-i18n="logs.filterAll">全部</option>
                        <option value="ok">OK</option>
                        <option value="fail">Fail</option>
                        <option value="reject">Reject</option>
                    </select>
                    <label data-i18n="logs.filterProtocol">协议</label>
                    <select id="pip-filter-protocol">
                        <option value="" data-i18n="logs.filterAll">全部</option>
                        <option value="TCP">TCP</option>
                        <option value="UDP">UDP</option>
                    </select>
                    <input type="text" id="pip-filter-search" placeholder="🔍" data-i18n-placeholder="logs.filterSearch">
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
            pipWindow.document.querySelectorAll('[data-i18n-placeholder]').forEach(el => {
                const key = el.dataset.i18nPlaceholder;
                el.placeholder = i18n.t(key);
            });

            const logsEl = pipWindow.document.getElementById('pip-logs');
            const countEl = pipWindow.document.getElementById('pip-count');
            const autoScrollEl = pipWindow.document.getElementById('pip-autoscroll');
            const baseUrl = window.location.origin;
            let lastSeq = 0;
            let pipLogBoot = null;
            let logCount = 0;

            function appendLog(e) {
                const date = new Date(e.time);
                const hours = String(date.getHours()).padStart(2, '0');
                const minutes = String(date.getMinutes()).padStart(2, '0');
                const seconds = String(date.getSeconds()).padStart(2, '0');
                const ms = String(date.getMilliseconds()).padStart(3, '0');
                const time = `${hours}:${minutes}:${seconds}.${ms}`;
                const icon = e.status === 'ok' ? '✓' : '✗';
                const inbound = e.inbound || '';
                let dstDisplay = `${e.dstAddr}:${e.dstPort}`;
                if (e.originalDstAddr && e.originalDstAddr !== e.dstAddr) {
                    dstDisplay = `${e.originalDstAddr} → ${e.dstAddr}:${e.dstPort}`;
                }
                let target = e.rule || e.proxy || 'DIRECT';
                if (e.actualProxy && e.actualProxy !== e.proxy) {
                    target += ' → ' + e.actualProxy;
                }
                let text = `${icon} [${inbound}] ${e.protocol} ${dstDisplay} → ${target}`;
                if (e.error) text += ` (${e.error})`;

                const searchFields = [e.dstAddr, e.originalDstAddr, e.rule, e.proxy, e.actualProxy, e.inbound, e.protocol, e.error, target].filter(Boolean).join(' ');

                const div = pipWindow.document.createElement('div');
                div.className = `log-line ${e.status}`;
                div.setAttribute('data-status', e.status);
                div.setAttribute('data-protocol', e.protocol || '');
                div.setAttribute('data-search', searchFields.toLowerCase());
                div.innerHTML = `<span class="log-time">[${time}]</span><span class="log-icon">${text.charAt(0)}</span>${text.substring(2)}`;
                logsEl.appendChild(div);
                logCount++;
                // Trim oldest logs to prevent unbounded DOM growth
                const MAX_PIP_LOGS = 1000;
                while (logsEl.children.length > MAX_PIP_LOGS) {
                    logsEl.removeChild(logsEl.firstChild);
                }
                countEl.textContent = i18n.t('pip.logCount').replace('{}', logCount);
                // Apply current filters to new entry
                applyFilters();
                // Scroll to bottom after each log for real-time updates
                scrollToBottom();
            }

            function applyFilters() {
                const statusVal = pipWindow.document.getElementById('pip-filter-status').value;
                const protoVal = pipWindow.document.getElementById('pip-filter-protocol').value;
                const searchVal = pipWindow.document.getElementById('pip-filter-search').value.toLowerCase();
                logsEl.querySelectorAll('.log-line').forEach(div => {
                    let show = true;
                    if (statusVal && div.getAttribute('data-status') !== statusVal) show = false;
                    if (protoVal && div.getAttribute('data-protocol') !== protoVal) show = false;
                    if (searchVal && !div.getAttribute('data-search').includes(searchVal)) show = false;
                    div.style.display = show ? '' : 'none';
                });
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
                    if (data.boot !== pipLogBoot) {
                        // Server restarted: our seq cursor is invalid.
                        pipLogBoot = data.boot;
                        lastSeq = 0;
                        fetchLogs(true);
                        return;
                    }
                    if (data.stale && !full) {
                        // Server sent a full snapshot in response to an
                        // incremental request (restart/resync): re-render
                        // as a full load to avoid duplicating lines.
                        full = true;
                    }
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

            // Filter event listeners
            ['pip-filter-status', 'pip-filter-protocol'].forEach(id => {
                pipWindow.document.getElementById(id).addEventListener('change', applyFilters);
            });
            pipWindow.document.getElementById('pip-filter-search').addEventListener('input', applyFilters);

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

// ========== Process Restart ==========
function restartProcess() {
    const msg = typeof i18n !== 'undefined' ? i18n.t('dash.confirmRestart') : 'Restart process? The service will be briefly unavailable.';
    openConfirmModal(msg, async function() {
        // Dangerous operation: require password confirmation
        const password = prompt(typeof i18n !== 'undefined' ? i18n.t('raw.passwordConfirm') : 'Enter password to confirm:');
        if (!password) return;

        try {
            const infoMsg = typeof i18n !== 'undefined' ? i18n.t('dash.restarting') : 'Restarting process...';
            showToast(infoMsg, 'info');
            const res = await fetch('./api/restart', {
                method: 'POST',
                headers: { 'X-Password-Confirm': password }
            });
            const data = await res.json();
            if (res.ok) {
                const okMsg = typeof i18n !== 'undefined' ? i18n.t('dash.restartOk') : 'Process restarting. Page will reload in a few seconds.';
                showToast('✅ ' + okMsg, 'success');
                // Wait for the process to restart, then reload the page
                setTimeout(() => location.reload(), 3000);
            } else {
                const failMsg = typeof i18n !== 'undefined' ? i18n.t('dash.restartFailed') : 'Restart failed';
                showToast('❌ ' + (data.error || failMsg), 'error');
            }
        } catch (err) {
            const netErr = typeof i18n !== 'undefined' ? i18n.t('toast.networkError') : 'Network error';
            showToast('❌ ' + netErr + ': ' + err.message, 'error');
        }
    });
}

// ========== Toast Notifications ==========
function showToast(message, type = 'success', duration = 3500, dismissible = true) {
    let container = document.querySelector('.toast-container');
    if (!container) {
        container = document.createElement('div');
        container.className = 'toast-container';
        document.body.appendChild(container);
    }

    const toast = document.createElement('div');
    toast.className = `toast toast-${type}`;
    const closeBtn = dismissible
        ? '<button onclick="this.parentElement.remove()" style="background:none;border:none;color:inherit;cursor:pointer;padding:0 0 0 8px;font-size:16px;opacity:0.7;line-height:1">&times;</button>'
        : '';
    toast.innerHTML = `<span style="flex:1">${escapeHtml(message)}</span>${closeBtn}`;
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

// ========== Password Modal ==========
let passwordModalCallback = null;

function openPasswordModal(onConfirm) {
    // Password confirmation removed - just execute callback directly
    if (onConfirm) onConfirm('');
}

function closePasswordModal() {
    const modal = document.getElementById('password-modal');
    if (modal) modal.classList.add('hidden');
    passwordModalCallback = null;
}

window.showPasswordConfirm = openPasswordModal;
window.closePasswordModal = closePasswordModal;

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
});
